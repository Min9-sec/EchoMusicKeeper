package proxy

import (
	"os"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
)

const (
	defaultBufferTarget = int64(2 * 1024 * 1024)
	minimumBufferTarget = int64(512 * 1024)
	maximumBufferTarget = int64(4 * 1024 * 1024)
	producerChunkSize   = 64 * 1024
	sessionIdleLifetime = 10 * time.Minute
)

var retryDelays = [...]time.Duration{
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	4 * time.Second,
}

type sessionIndex interface {
	OpenFile(relativePath string, flag int, perm os.FileMode) (*os.File, error)
	StatFile(relativePath string) (os.FileInfo, error)
	RemoveFile(relativePath string) error
	RenameFile(oldPath, newPath string) error
	Upsert(key string, entry cache.Entry) error
	Touch(key string, at time.Time) error
}

type sessionOptions struct {
	responseHeaderTimeout time.Duration
	bodyIdleTimeout       time.Duration
	startupWait           time.Duration
	retryDelays           []time.Duration
	jitter                func(time.Duration) time.Duration
	after                 func(time.Duration) <-chan time.Time
	now                   func() time.Time
	checkpointBytes       int64
	checkpointInterval    time.Duration
	writeAt               func(*os.File, []byte, int64) (int, error)
	cancelClaimBarrier    func()
}

func defaultSessionOptions() sessionOptions {
	return sessionOptions{
		responseHeaderTimeout: connectionTimeout,
		bodyIdleTimeout:       connectionTimeout,
		startupWait:           5 * time.Second,
		retryDelays:           append([]time.Duration(nil), retryDelays[:]...),
		jitter:                jitter,
		after:                 time.After,
		now:                   time.Now,
		checkpointBytes:       1024 * 1024,
		checkpointInterval:    time.Second,
		writeAt: func(file *os.File, value []byte, offset int64) (int, error) {
			return file.WriteAt(value, offset)
		},
	}
}

func normalizeSessionOptions(options sessionOptions) sessionOptions {
	defaults := defaultSessionOptions()
	if options.responseHeaderTimeout <= 0 {
		options.responseHeaderTimeout = defaults.responseHeaderTimeout
	}
	if options.bodyIdleTimeout <= 0 {
		options.bodyIdleTimeout = defaults.bodyIdleTimeout
	}
	if options.startupWait <= 0 {
		options.startupWait = defaults.startupWait
	}
	if len(options.retryDelays) == 0 {
		options.retryDelays = defaults.retryDelays
	} else {
		options.retryDelays = append([]time.Duration(nil), options.retryDelays...)
	}
	if options.jitter == nil {
		options.jitter = defaults.jitter
	}
	if options.after == nil {
		options.after = defaults.after
	}
	if options.now == nil {
		options.now = defaults.now
	}
	if options.checkpointBytes <= 0 {
		options.checkpointBytes = defaults.checkpointBytes
	}
	if options.checkpointInterval <= 0 {
		options.checkpointInterval = defaults.checkpointInterval
	}
	if options.writeAt == nil {
		options.writeAt = defaults.writeAt
	}
	return options
}
