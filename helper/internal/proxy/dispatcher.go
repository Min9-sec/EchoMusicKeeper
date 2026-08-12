package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func (session *Session) dispatch() {
	defer session.finish()
	for {
		release, err := session.manager.acquireReference(session.key, session, referenceProducer)
		if err != nil {
			return
		}
		interval, remoteURL, action := session.nextInterval()
		switch action {
		case dispatchStop:
			release()
			return
		case dispatchWait:
			release()
			<-session.controlWake
			continue
		case dispatchFinalize:
			if err := session.finalize(); err != nil {
				session.setFailed("finalize cache file")
				release()
				continue
			}
			release()
			return
		}

		result := session.fetch(interval, remoteURL)
		stopped := session.handleFetchResult(result)
		release()
		if stopped {
			return
		}
	}
}

type dispatchAction int

const (
	dispatchFetch dispatchAction = iota
	dispatchWait
	dispatchFinalize
	dispatchStop
)

func (session *Session) nextInterval() (model.ByteRange, string, dispatchAction) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.stopped || session.expired || session.state == model.TaskCompleted {
		return model.ByteRange{}, "", dispatchStop
	}
	if session.totalBytes > 0 {
		missing := cache.Missing(session.totalBytes, session.ranges)
		if len(missing) == 0 {
			session.producer = true
			return model.ByteRange{}, "", dispatchFinalize
		}
	}
	if session.remoteURL == "" || session.state == model.TaskNeedsURL || session.state == model.TaskFailed {
		session.producer = false
		return model.ByteRange{}, "", dispatchWait
	}
	if session.totalBytes > 0 {
		missing := cache.Missing(session.totalBytes, session.ranges)
		session.priorities = remainingDemands(session.priorities, session.ranges)
		for _, demand := range session.priorities {
			if interval, ok := demandInterval(demand, missing); ok {
				return session.beginFetchLocked(interval)
			}
		}
		return session.beginFetchLocked(missing[0])
	}
	return session.beginFetchLocked(model.ByteRange{Start: 0})
}

func (session *Session) beginFetchLocked(interval model.ByteRange) (model.ByteRange, string, dispatchAction) {
	select {
	case <-session.controlWake:
	default:
	}
	select {
	case <-session.priorityWake:
	default:
	}
	requestContext, cancel := context.WithCancel(context.Background())
	session.requestCtx = requestContext
	session.cancel = cancel
	session.producer = true
	session.activeNext = interval.Start
	session.updatedAt = session.options.now().UTC()
	if coveredBytes(session.ranges) == 0 {
		session.state = model.TaskBuffering
	} else {
		session.state = model.TaskRunning
	}
	return interval, session.remoteURL, dispatchFetch
}

func (session *Session) fetch(interval model.ByteRange, remoteURL string) fetchResult {
	session.mu.Lock()
	requestContext := session.requestCtx
	session.mu.Unlock()
	if requestContext == nil {
		return fetchResult{kind: fetchCanceled}
	}
	headerContext, cancelRequest := context.WithCancel(requestContext)
	defer cancelRequest()

	rangeHeader := fmt.Sprintf("bytes=%d-", interval.Start)
	if interval.End > interval.Start {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", interval.Start, interval.End-1)
	}
	request, err := http.NewRequestWithContext(headerContext, http.MethodGet, remoteURL, nil)
	if err != nil {
		return fetchResult{kind: fetchFatal, message: "create upstream request"}
	}
	request.Header.Set("Range", rangeHeader)
	var headerTimedOut atomic.Bool
	headerTimerDone := make(chan struct{})
	headerTimer := time.AfterFunc(session.options.responseHeaderTimeout, func() {
		headerTimedOut.Store(true)
		cancelRequest()
		close(headerTimerDone)
	})
	response, err := session.client.Do(request)
	if !headerTimer.Stop() {
		<-headerTimerDone
	}
	if headerTimedOut.Load() {
		if response != nil {
			_ = response.Body.Close()
		}
		return fetchResult{kind: fetchRetryable, message: "upstream response headers timed out"}
	}
	if err != nil {
		if requestContext.Err() != nil {
			return fetchResult{kind: fetchCanceled}
		}
		return fetchResult{kind: fetchRetryable, message: "upstream transport failure"}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fetchResult{kind: fetchNeedsURL, message: "upstream URL requires refresh"}
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return fetchResult{kind: fetchRetryable, message: "upstream temporarily unavailable"}
	}
	if response.StatusCode != http.StatusPartialContent {
		return fetchResult{kind: fetchFatal, message: "upstream did not honor byte range"}
	}

	responseRange, total, err := parseContentRange(response.Header.Get("Content-Range"))
	if err != nil || responseRange.Start != interval.Start || (interval.End > interval.Start && responseRange.End != interval.End) {
		return fetchResult{kind: fetchFatal, message: "invalid upstream content range"}
	}
	expectedLength := responseRange.End - responseRange.Start
	if response.ContentLength != expectedLength {
		return fetchResult{kind: fetchFatal, message: "invalid upstream content length"}
	}

	session.mu.Lock()
	learnedTotal := session.totalBytes == 0
	if session.totalBytes != 0 && session.totalBytes != total {
		session.mu.Unlock()
		return fetchResult{kind: fetchFatal, message: "upstream length changed"}
	}
	session.totalBytes = total
	if mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type")); parseErr == nil && mediaType != "" {
		session.contentType = mediaType
	}
	session.signalLocked()
	session.mu.Unlock()
	if learnedTotal {
		if err := session.checkpointPartial(); err != nil {
			return fetchResult{kind: fetchFatal, message: "persist upstream length"}
		}
	}

	file, err := session.index.OpenFile(session.partPath, os.O_RDWR, 0)
	if err != nil {
		return fetchResult{kind: fetchFatal, message: "open partial cache file"}
	}
	defer file.Close()

	buffer := make([]byte, producerChunkSize)
	offset := responseRange.Start
	for offset < responseRange.End {
		want := min(int64(len(buffer)), responseRange.End-offset)
		var bodyTimedOut atomic.Bool
		bodyTimerDone := make(chan struct{})
		bodyTimer := time.AfterFunc(session.options.bodyIdleTimeout, func() {
			bodyTimedOut.Store(true)
			cancelRequest()
			close(bodyTimerDone)
		})
		read, readErr := response.Body.Read(buffer[:want])
		if !bodyTimer.Stop() {
			<-bodyTimerDone
		}
		if read > 0 {
			if int64(read) > responseRange.End-offset {
				return fetchResult{kind: fetchFatal, message: "upstream exceeded content range"}
			}
			written, writeErr := session.writeChunk(file, offset, buffer[:read])
			if written > 0 {
				offset += int64(written)
			}
			if writeErr != nil || written != read {
				return fetchResult{kind: fetchFatal, message: "write partial cache file"}
			}
		}
		if bodyTimedOut.Load() {
			return fetchResult{kind: fetchRetryable, message: "upstream body idle timeout"}
		}
		if readErr != nil {
			if requestContext.Err() != nil {
				return fetchResult{kind: fetchCanceled}
			}
			if errors.Is(readErr, io.EOF) && offset == responseRange.End {
				break
			}
			return fetchResult{kind: fetchRetryable, message: "upstream body interrupted"}
		}
		if read == 0 {
			return fetchResult{kind: fetchRetryable, message: "upstream body stalled"}
		}
	}
	return fetchResult{kind: fetchOK}
}

func (session *Session) writeChunk(file *os.File, offset int64, data []byte) (int, error) {
	written, err := session.options.writeAt(file, data, offset)
	if written > 0 {
		session.mu.Lock()
		session.ranges = cache.Merge(session.ranges, model.ByteRange{Start: offset, End: offset + int64(written)})
		session.activeNext = offset + int64(written)
		session.bytesSinceCheckpoint += int64(written)
		session.updatedAt = session.options.now().UTC()
		session.signalLocked()
		session.mu.Unlock()
		if persistErr := session.checkpointPartialIfDue(file); persistErr != nil && err == nil {
			err = persistErr
		}
	}
	return written, err
}

type fetchKind int

const (
	fetchOK fetchKind = iota
	fetchCanceled
	fetchNeedsURL
	fetchRetryable
	fetchFatal
)

type fetchResult struct {
	kind    fetchKind
	message string
}

func (session *Session) handleFetchResult(result fetchResult) bool {
	if result.kind != fetchOK {
		if err := session.checkpointPartial(); err != nil {
			result = fetchResult{kind: fetchFatal, message: "checkpoint partial cache"}
		}
	}
	session.mu.Lock()
	if session.cancel != nil {
		session.cancel = nil
	}
	session.requestCtx = nil
	session.activeNext = 0
	session.producer = false
	if session.stopped || session.expired {
		session.signalLocked()
		session.mu.Unlock()
		return true
	}
	switch result.kind {
	case fetchOK:
		session.retryCount = 0
		session.lastError = ""
		session.signalLocked()
		session.mu.Unlock()
		return false
	case fetchCanceled:
		session.signalLocked()
		session.mu.Unlock()
		return false
	case fetchNeedsURL:
		session.remoteURL = ""
		session.retryCount = 0
		session.state = model.TaskNeedsURL
		session.lastError = result.message
		session.updatedAt = session.options.now().UTC()
		session.signalLocked()
		session.mu.Unlock()
		return false
	case fetchRetryable:
		if session.retryCount < len(session.options.retryDelays) {
			delay := session.options.jitter(session.options.retryDelays[session.retryCount])
			session.retryCount++
			session.producer = true
			session.lastError = result.message
			session.updatedAt = session.options.now().UTC()
			session.signalLocked()
			session.mu.Unlock()
			select {
			case <-session.options.after(delay):
			case <-session.controlWake:
			}
			return false
		}
	}
	session.state = model.TaskFailed
	session.remoteURL = ""
	session.lastError = result.message
	session.updatedAt = session.options.now().UTC()
	session.signalLocked()
	session.mu.Unlock()
	return false
}

func (session *Session) setFailed(message string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.producer = false
	if session.stopped || session.expired || session.state == model.TaskCanceled || session.state == model.TaskCompleted {
		session.signalLocked()
		return
	}
	session.state = model.TaskFailed
	session.remoteURL = ""
	session.lastError = message
	session.updatedAt = session.options.now().UTC()
	session.signalLocked()
}
