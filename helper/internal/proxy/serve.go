package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func (session *Session) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	release, ok := session.readerStarted()
	if !ok {
		http.Error(writer, "playback session expired", http.StatusGone)
		return
	}
	defer session.readerFinished(release)

	deadline := time.Now().Add(session.options.startupWait)
	total, ok := session.waitForTotal(request.Context(), deadline)
	if !ok {
		http.Error(writer, "cache stream is unavailable", http.StatusServiceUnavailable)
		return
	}
	requested, err := cache.ParseSingleRange(request.Header.Get("Range"), total)
	if err != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	target := min(requested.End, requested.Start+session.bufferTarget(total))
	session.requestDemand(requested.Start, target)
	if err := session.waitForCoverage(request.Context(), model.ByteRange{Start: requested.Start, End: target}, deadline); err != nil {
		http.Error(writer, "cache stream is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !session.positionAvailable(requested.Start) {
		http.Error(writer, "cache stream is unavailable", http.StatusServiceUnavailable)
		return
	}
	file, err := session.openCurrentFile(request.Context(), deadline)
	if err != nil {
		http.Error(writer, "cache stream is unavailable", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()

	contentType := session.getContentType()
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.Start, requested.End-1, total))
	writer.Header().Set("Content-Length", strconv.FormatInt(requested.End-requested.Start, 10))
	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(http.StatusPartialContent)
	session.streamRange(writer, request, requested, file)
}

func (session *Session) streamRange(writer http.ResponseWriter, request *http.Request, requested model.ByteRange, file *os.File) {
	buffer := make([]byte, producerChunkSize)
	position := requested.Start
	for position < requested.End {
		session.mu.Lock()
		availableEnd := min(contiguousEnd(session.ranges, position), requested.End)
		terminal := session.stopped || session.expired || session.state == model.TaskNeedsURL || session.state == model.TaskFailed || session.state == model.TaskCanceled
		changed := session.changed
		session.mu.Unlock()

		if availableEnd <= position {
			if terminal {
				return
			}
			target := min(requested.End, position+session.bufferTarget(session.knownTotal()))
			session.requestDemand(position, target)
			select {
			case <-request.Context().Done():
				return
			case <-changed:
			}
			continue
		}

		want := min(int64(len(buffer)), availableEnd-position)
		read, err := file.ReadAt(buffer[:want], position)
		if read > 0 {
			written, writeErr := writer.Write(buffer[:read])
			position += int64(written)
			if writeErr != nil || written != read {
				return
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		if read == 0 {
			select {
			case <-request.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}
}

func (session *Session) waitForTotal(ctx context.Context, deadline time.Time) (int64, bool) {
	for {
		session.mu.Lock()
		total := session.totalBytes
		terminal := session.stopped || session.expired || session.state == model.TaskNeedsURL || session.state == model.TaskFailed || session.state == model.TaskCanceled
		changed := session.changed
		session.mu.Unlock()
		if total > 0 {
			return total, true
		}
		if terminal {
			return 0, false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, false
		case <-timer.C:
			return 0, false
		case <-changed:
			timer.Stop()
		}
	}
}

func (session *Session) waitForCoverage(ctx context.Context, requested model.ByteRange, deadline time.Time) error {
	for {
		session.mu.Lock()
		covered := contiguousEnd(session.ranges, requested.Start) >= requested.End
		terminal := session.stopped || session.expired || session.state == model.TaskNeedsURL || session.state == model.TaskFailed || session.state == model.TaskCanceled
		changed := session.changed
		session.mu.Unlock()
		if covered {
			return nil
		}
		if terminal {
			return fmt.Errorf("cache stream entered terminal state before startup coverage")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if session.positionAvailable(requested.Start) {
				return nil
			}
			return fmt.Errorf("cache stream has no startup bytes")
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-changed:
			timer.Stop()
		}
	}
}

func (session *Session) requestDemand(start, end int64) {
	if end <= start {
		return
	}
	session.mu.Lock()
	missingStart, missing := firstMissingPosition(session.ranges, start, end)
	if !missing || session.stopped || session.expired {
		session.mu.Unlock()
		return
	}
	next := []byteDemand{{start: start, end: end}}
	for _, demand := range session.priorities {
		if demand.start != start {
			next = append(next, demand)
		}
	}
	session.priorities = next
	shouldCancel := session.cancel != nil && missingStart != session.activeNext
	if shouldCancel {
		session.cancel()
	}
	session.wakePriorityLocked()
	session.mu.Unlock()
}

func (session *Session) bufferTarget(total int64) int64 {
	session.mu.Lock()
	duration := session.track.DurationSeconds
	session.mu.Unlock()
	if duration <= 0 || total <= 0 {
		return min(defaultBufferTarget, max(total, defaultBufferTarget))
	}
	target := int64(float64(total) / max(duration, 1) * 15)
	return min(max(target, minimumBufferTarget), maximumBufferTarget)
}

func (session *Session) readerStarted() (func(), bool) {
	release, err := session.manager.acquireReference(session.key, session, referenceReader)
	if err != nil {
		return nil, false
	}
	session.mu.Lock()
	if session.stopped || session.expired || session.playToken == "" {
		session.mu.Unlock()
		release()
		return nil, false
	}
	if session.expiryTimer != nil {
		session.expiryTimer.Stop()
		session.expiryTimer = nil
	}
	session.readers++
	session.updatedAt = session.options.now().UTC()
	session.mu.Unlock()
	return release, true
}

func (session *Session) readerFinished(release func()) {
	session.mu.Lock()
	if session.readers > 0 {
		session.readers--
	}
	completed := session.state == model.TaskCompleted
	session.updatedAt = session.options.now().UTC()
	if completed && session.readers == 0 {
		session.scheduleExpiryLocked()
	}
	session.mu.Unlock()
	if completed {
		_ = session.index.Touch(session.key, session.options.now().UTC())
	}
	release()
}

func (session *Session) scheduleExpiryLocked() {
	if session.state != model.TaskCompleted || session.readers != 0 || session.stopped || session.expired {
		return
	}
	if session.expiryTimer != nil {
		session.expiryTimer.Stop()
	}
	session.expiryTimer = time.AfterFunc(sessionIdleLifetime, func() {
		session.mu.Lock()
		if session.readers != 0 || session.state != model.TaskCompleted || session.stopped {
			session.mu.Unlock()
			return
		}
		session.expired = true
		session.playToken = ""
		session.signalLocked()
		session.mu.Unlock()
		session.manager.expireSession(session.key, session)
	})
}

func (session *Session) canExpire() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.expired && session.readers == 0 && !session.producer && session.exports == 0 && session.migrations == 0
}

func (session *Session) protected() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	activeProducer := session.producer || (session.running && session.remoteURL != "" &&
		(session.state == model.TaskBuffering || session.state == model.TaskRunning))
	return activeProducer || session.readers > 0 || session.exports > 0 || session.migrations > 0
}

func (session *Session) task() model.Task {
	session.mu.Lock()
	defer session.mu.Unlock()
	downloaded := coveredBytes(session.ranges)
	elapsed := time.Since(session.createdAt).Seconds()
	var bytesPerSecond int64
	if elapsed > 0 {
		bytesPerSecond = int64(float64(downloaded) / elapsed)
	}
	outputPath := ""
	if session.state == model.TaskCompleted {
		outputPath = session.finalPath
	}
	return model.Task{
		ID:              session.id,
		Kind:            model.TaskKindCache,
		Track:           session.track,
		State:           session.state,
		DownloadedBytes: downloaded,
		TotalBytes:      session.totalBytes,
		BytesPerSecond:  bytesPerSecond,
		Pausable:        false,
		Cancelable:      session.state != model.TaskCompleted && session.state != model.TaskCanceled,
		Retryable:       session.state == model.TaskNeedsURL || session.state == model.TaskFailed,
		Error:           session.lastError,
		OutputPath:      outputPath,
		UpdatedAt:       session.updatedAt,
	}
}

func (session *Session) signalLocked() {
	close(session.changed)
	session.changed = make(chan struct{})
}

func (session *Session) finish() {
	session.doneOnce.Do(func() { close(session.done) })
}

func (session *Session) wakeControlLocked() {
	select {
	case session.controlWake <- struct{}{}:
	default:
	}
}

func (session *Session) wakePriorityLocked() {
	select {
	case session.priorityWake <- struct{}{}:
	default:
	}
}

func (session *Session) getContentType() string {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.contentType != "" {
		return session.contentType
	}
	return "application/octet-stream"
}

func (session *Session) knownTotal() int64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.totalBytes
}

func (session *Session) positionAvailable(position int64) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return contiguousEnd(session.ranges, position) > position
}

func (session *Session) openCurrentFile(ctx context.Context, deadline time.Time) (*os.File, error) {
	for {
		session.mu.Lock()
		path := session.currentPath
		changed := session.changed
		session.mu.Unlock()
		file, err := session.index.OpenFile(path, os.O_RDONLY, 0)
		if err == nil {
			return file, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
			return nil, err
		}
	}
}
