package testserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

const defaultChunkSize = 32 * 1024

// RangeRecorder records requested half-open intervals and controls one-shot
// upstream faults used by proxy tests.
type RangeRecorder struct {
	mu sync.Mutex

	requests        []model.ByteRange
	chunkSize       int
	chunkDelay      time.Duration
	forbidden       int
	disconnect      int
	disconnectAfter int64
	active          int
	maxActive       int
}

func NewRangeServer(data []byte) (*httptest.Server, *RangeRecorder) {
	recorder := &RangeRecorder{chunkSize: defaultChunkSize}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requested, err := cache.ParseSingleRange(request.Header.Get("Range"), int64(len(data)))
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			http.Error(w, "a valid Range header is required", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, requested)
		recorder.active++
		if recorder.active > recorder.maxActive {
			recorder.maxActive = recorder.active
		}
		forbidden := recorder.forbidden > 0
		if forbidden {
			recorder.forbidden--
		}
		disconnect := !forbidden && recorder.disconnect > 0
		if disconnect {
			recorder.disconnect--
		}
		disconnectAfter := recorder.disconnectAfter
		chunkSize := recorder.chunkSize
		delay := recorder.chunkDelay
		recorder.mu.Unlock()
		defer func() {
			recorder.mu.Lock()
			recorder.active--
			recorder.mu.Unlock()
		}()

		if forbidden {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}

		length := requested.End - requested.Start
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.Start, requested.End-1, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if disconnect {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusPartialContent)

		if chunkSize <= 0 {
			chunkSize = defaultChunkSize
		}
		if disconnectAfter <= 0 || disconnectAfter >= length {
			disconnectAfter = max(int64(chunkSize), length/2)
		}
		written := int64(0)
		for offset := requested.Start; offset < requested.End; {
			end := min(offset+int64(chunkSize), requested.End)
			if disconnect && written < disconnectAfter && written+(end-offset) > disconnectAfter {
				end = offset + disconnectAfter - written
			}
			if end <= offset {
				return
			}
			if _, err := w.Write(data[offset:end]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			written += end - offset
			offset = end
			if disconnect && written >= disconnectAfter {
				return
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}))
	return server, recorder
}

func (recorder *RangeRecorder) RequestCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.requests)
}

func (recorder *RangeRecorder) Count() int {
	return recorder.RequestCount()
}

func (recorder *RangeRecorder) RequestedIntervals() []model.ByteRange {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]model.ByteRange(nil), recorder.requests...)
}

func (recorder *RangeRecorder) Ranges() []model.ByteRange {
	return recorder.RequestedIntervals()
}

func (recorder *RangeRecorder) MaxActive() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.maxActive
}

func (recorder *RangeRecorder) SetChunkDelay(delay time.Duration) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.chunkDelay = delay
}

func (recorder *RangeRecorder) SetChunkSize(size int) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.chunkSize = size
}

func (recorder *RangeRecorder) FailNext403() {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.forbidden++
}

func (recorder *RangeRecorder) DisconnectNext() {
	recorder.DisconnectNextAfter(0)
}

func (recorder *RangeRecorder) DisconnectNextAfter(bytes int64) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.disconnect++
	recorder.disconnectAfter = bytes
}
