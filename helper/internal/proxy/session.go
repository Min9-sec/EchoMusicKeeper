package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

type byteDemand struct {
	start int64
	end   int64
}

type Session struct {
	mu           sync.Mutex
	checkpointMu sync.Mutex

	manager *Manager
	index   sessionIndex
	client  *http.Client
	options sessionOptions
	key     string

	id          string
	playToken   string
	track       model.TrackInfo
	remoteURL   string
	partPath    string
	finalPath   string
	currentPath string
	totalBytes  int64
	ranges      []model.ByteRange
	state       model.TaskState
	lastError   string
	cancel      context.CancelFunc
	requestCtx  context.Context

	contentType          string
	createdAt            time.Time
	completedAt          time.Time
	updatedAt            time.Time
	activeNext           int64
	priorities           []byteDemand
	retryCount           int
	producer             bool
	readers              int
	exports              int
	migrations           int
	running              bool
	stopped              bool
	expired              bool
	bytesSinceCheckpoint int64
	lastCheckpointAt     time.Time

	controlWake     chan struct{}
	priorityWake    chan struct{}
	changed         chan struct{}
	done            chan struct{}
	shutdownDone    chan struct{}
	doneOnce        sync.Once
	shutdownStarted bool
	shutdownErr     error
	expiryTimer     *time.Timer
}

func newSession(manager *Manager, key string, entry cache.Entry, remoteURL string, state model.TaskState) (*Session, error) {
	id, err := newSecret()
	if err != nil {
		return nil, err
	}
	playToken, err := newSecret()
	if err != nil {
		return nil, err
	}
	now := manager.options.now().UTC()
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	session := &Session{
		manager:          manager,
		index:            manager.index,
		client:           manager.client,
		options:          manager.options,
		key:              key,
		id:               id,
		playToken:        playToken,
		track:            entry.Track,
		remoteURL:        remoteURL,
		partPath:         partialPath(key),
		totalBytes:       entry.TotalBytes,
		ranges:           cache.Merge(entry.Ranges, model.ByteRange{}),
		state:            state,
		createdAt:        createdAt,
		completedAt:      entry.CompletedAt,
		updatedAt:        now,
		lastCheckpointAt: now,
		controlWake:      make(chan struct{}, 1),
		priorityWake:     make(chan struct{}, 1),
		changed:          make(chan struct{}),
		done:             make(chan struct{}),
		shutdownDone:     make(chan struct{}),
	}
	if entry.Complete {
		session.currentPath = entry.RelativePath
		session.finalPath = entry.RelativePath
		session.totalBytes = entry.Size
		session.ranges = []model.ByteRange{{Start: 0, End: entry.Size}}
		session.state = model.TaskCompleted
		session.finish()
		session.scheduleExpiryLocked()
		return session, nil
	}
	file, err := session.index.OpenFile(session.partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open partial cache file: %w", err)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, fmt.Errorf("inspect partial cache file: %w", statErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close partial cache file: %w", closeErr)
	}
	session.ranges = sanitizeRanges(session.ranges, info.Size(), session.totalBytes)
	session.currentPath = session.partPath
	return session, nil
}

func (session *Session) ID() string {
	return session.id
}

func (session *Session) Key() string {
	return session.key
}

func (session *Session) PlaybackToken() string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.playToken
}

func (session *Session) authorized(token string) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return !session.expired && !session.stopped && secretsEqual(session.playToken, token)
}

func (session *Session) start() {
	session.mu.Lock()
	if session.running || session.state == model.TaskCompleted || session.stopped {
		session.mu.Unlock()
		return
	}
	session.running = true
	session.mu.Unlock()
	go session.dispatch()
}

func (session *Session) updateURL(remoteURL string) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.stopped || session.expired {
		return fmt.Errorf("cache session is closed")
	}
	if session.state == model.TaskCompleted {
		return nil
	}
	if session.remoteURL == remoteURL && (session.state == model.TaskBuffering || session.state == model.TaskRunning) {
		return nil
	}
	session.remoteURL = remoteURL
	session.retryCount = 0
	session.lastError = ""
	session.state = model.TaskBuffering
	session.updatedAt = session.options.now().UTC()
	if session.cancel != nil {
		session.cancel()
	}
	session.signalLocked()
	session.wakeControlLocked()
	return nil
}

func (session *Session) cancelTask() error {
	claim, err := session.claimCancellation()
	if err != nil {
		return err
	}
	return session.completeShutdown(claim)
}

func (session *Session) close(markCanceled bool) error {
	claim, err := session.claimShutdown(markCanceled, false)
	if err != nil {
		return err
	}
	return session.completeShutdown(claim)
}

type shutdownClaim struct {
	owner bool
	done  <-chan struct{}
}

func (session *Session) claimCancellation() (shutdownClaim, error) {
	if session.options.cancelClaimBarrier != nil {
		session.options.cancelClaimBarrier()
	}
	return session.claimShutdown(true, true)
}

func (session *Session) claimShutdown(markCanceled, rejectCompleted bool) (shutdownClaim, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if rejectCompleted && session.state == model.TaskCompleted {
		return shutdownClaim{}, fmt.Errorf("completed task cannot be canceled")
	}
	owner := !session.shutdownStarted
	if owner {
		session.shutdownStarted = true
		if session.expiryTimer != nil {
			session.expiryTimer.Stop()
		}
		session.stopped = true
		session.remoteURL = ""
		session.playToken = ""
		if markCanceled && session.state != model.TaskCompleted {
			session.state = model.TaskCanceled
			session.updatedAt = session.options.now().UTC()
		}
		if session.cancel != nil {
			session.cancel()
		}
		session.signalLocked()
		session.wakeControlLocked()
		if !session.running {
			session.finish()
		}
	} else if markCanceled && session.state != model.TaskCompleted && session.state != model.TaskCanceled {
		session.state = model.TaskCanceled
		session.updatedAt = session.options.now().UTC()
		session.signalLocked()
	}
	return shutdownClaim{owner: owner, done: session.shutdownDone}, nil
}

func (session *Session) completeShutdown(claim shutdownClaim) error {
	if claim.owner {
		<-session.done
		shutdownErr := session.syncPartial()
		session.mu.Lock()
		session.shutdownErr = shutdownErr
		close(session.shutdownDone)
		session.mu.Unlock()
	}
	<-claim.done
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.shutdownErr
}

func (session *Session) canceledShutdown() (<-chan struct{}, bool) {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.shutdownDone, session.state == model.TaskCanceled
}

func (session *Session) setCurrentPath(path string) {
	session.mu.Lock()
	if session.currentPath != path {
		session.currentPath = path
		session.signalLocked()
	}
	session.mu.Unlock()
}
