package proxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	cacheexport "github.com/Min9-sec/EchoMusicKeeper/helper/internal/export"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

const (
	connectionTimeout      = 10 * time.Second
	maxCanceledTaskHistory = 128
	defaultCacheLimit      = int64(1_073_741_824)
)

type Manager struct {
	mu                    sync.Mutex
	index                 *cache.Index
	client                *http.Client
	options               sessionOptions
	sessions              map[string]*Session
	history               map[string]model.Task
	historyOrder          []string
	downloads             map[string]*downloadTask
	references            map[string]int
	condition             *sync.Cond
	cleaning              bool
	cleaningDone          chan struct{}
	migrating             bool
	migrationDone         chan struct{}
	cacheLimit            int64
	downloadRoot          string
	downloadRoots         []string
	downloadRecords       map[string]string
	downloadRootInfo      map[string]os.FileInfo
	copyDownload          downloadCopyFunc
	cleanupDownload       downloadCleanupFunc
	beforeDownloadDelete  func()
	beforeDownloadPublish func()
	beforeOldRootDelete   func()
	beforeStagingCleanup  func(string)
	afterPublishedCleanup func()
	afterDestinationCheck func()
	afterDestinationClone func()
	afterOldRootCleanup   func()
	afterStagingCleanup   func()
	controlNow            func() time.Time
	statCacheEntry        func(*cache.Index, string) (os.FileInfo, error)
	touchCacheEntry       func(*cache.Index, string, time.Time) error
	afterCompleteLookup   func()
	migrationOps          migrationOperations
	unavailableErr        error
	closed                bool
	closeOnce             sync.Once
	closeErr              error
}

func NewManager(index *cache.Index, client *http.Client) *Manager {
	return newManagerWithOptions(index, client, defaultSessionOptions())
}

// NewManagerForTesting substitutes the final dial only after policy validation.
// Runtime code must use NewManager so every validated address is the one dialed.
func NewManagerForTesting(index *cache.Index, client *http.Client) *Manager {
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		panic("test HTTP client requires an *http.Transport with DialContext")
	}
	secured := secureHTTPClientWithNetwork(client, nil, transport.DialContext)
	return newManagerWithPreparedClient(index, secured, defaultSessionOptions())
}

func newManagerWithOptions(index *cache.Index, client *http.Client, options sessionOptions) *Manager {
	return newManagerWithPreparedClient(index, secureHTTPClient(client), options)
}

func newManagerWithPreparedClient(index *cache.Index, client *http.Client, options sessionOptions) *Manager {
	options = normalizeSessionOptions(options)
	manager := &Manager{
		index:            index,
		client:           client,
		options:          options,
		sessions:         make(map[string]*Session),
		history:          make(map[string]model.Task),
		downloads:        make(map[string]*downloadTask),
		references:       make(map[string]int),
		downloadRecords:  make(map[string]string),
		downloadRootInfo: make(map[string]os.FileInfo),
		cacheLimit:       defaultCacheLimit,
		controlNow:       time.Now,
		statCacheEntry: func(index *cache.Index, relativePath string) (os.FileInfo, error) {
			return index.StatFile(relativePath)
		},
		touchCacheEntry: func(index *cache.Index, key string, at time.Time) error {
			return index.Touch(key, at)
		},
	}
	manager.copyDownload = cacheexport.CopyAtomicTo
	manager.cleanupDownload = func(directory *security.Directory, name string) error {
		return directory.Cleanup(name)
	}
	manager.migrationOps = defaultMigrationOperations()
	manager.condition = sync.NewCond(&manager.mu)
	if index == nil {
		return manager
	}
	for key, entry := range index.Snapshot() {
		if entry.Complete {
			continue
		}
		actualKey, track, err := normalizeTrack(entry.Track)
		if err != nil || actualKey != key {
			continue
		}
		partPath := partialPath(key)
		info, err := index.StatFile(partPath)
		if err != nil {
			continue
		}
		entry.Track = track
		entry.RelativePath = partPath
		entry.Ranges = sanitizeRanges(entry.Ranges, info.Size(), entry.TotalBytes)
		session, err := newSession(manager, key, entry, "", model.TaskNeedsURL)
		if err != nil {
			continue
		}
		_ = session.checkpointPartial()
		manager.sessions[key] = session
		session.start()
	}
	return manager
}

func (manager *Manager) CreateOrReuse(ctx context.Context, request model.CreateSessionRequest) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsedURL, err := security.ValidateRemoteURL(request.RemoteURL)
	if err != nil {
		return nil, err
	}
	key, track, err := normalizeTrack(request.Track)
	if err != nil {
		return nil, err
	}

	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, fmt.Errorf("proxy manager is closed")
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache manager unavailable: %w", err)
		}
		if blocked := manager.blockedDoneLocked(); blocked != nil {
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-blocked:
			}
			continue
		}
		if manager.index == nil {
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache index is required")
		}
		if session := manager.sessions[key]; session != nil {
			shutdownDone, canceled := session.canceledShutdown()
			if canceled {
				manager.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-shutdownDone:
				}
				manager.mu.Lock()
				if manager.sessions[key] == session {
					manager.archiveCanceledLocked(key, session)
				}
				manager.mu.Unlock()
				continue
			}
			if err := session.updateURL(parsedURL.String()); err != nil {
				manager.mu.Unlock()
				return nil, err
			}
			manager.mu.Unlock()
			return session, nil
		}

		entry, found, lookupErr := manager.index.Lookup(key)
		if lookupErr != nil {
			manager.mu.Unlock()
			return nil, lookupErr
		}
		if found {
			track = mergeStoredTrack(entry.Track, track)
			entry.Track = track
		} else {
			now := time.Now().UTC()
			entry = cache.Entry{
				Track:          track,
				RelativePath:   partialPath(key),
				CreatedAt:      now,
				LastAccessedAt: now,
			}
		}
		state := model.TaskBuffering
		remoteURL := parsedURL.String()
		if entry.Complete {
			state = model.TaskCompleted
			remoteURL = ""
		}
		session, err := newSession(manager, key, entry, remoteURL, state)
		if err != nil {
			manager.mu.Unlock()
			return nil, err
		}
		if !entry.Complete {
			if err := session.checkpointPartial(); err != nil {
				_ = session.close(false)
				manager.mu.Unlock()
				return nil, err
			}
		}
		manager.sessions[key] = session
		session.start()
		manager.mu.Unlock()
		return session, nil
	}
}

func (manager *Manager) Lookup(request model.CacheLookupRequest) (model.CacheLookup, error) {
	index, err := manager.availableIndex()
	if err != nil {
		return model.CacheLookup{}, err
	}
	if index == nil {
		return model.CacheLookup{}, fmt.Errorf("cache index is required")
	}
	var entry cache.Entry
	var found bool
	var lookupErr error
	if request.Key != "" {
		key, err := validateCacheKey(request.Key)
		if err != nil {
			return model.CacheLookup{}, err
		}
		entry, found, lookupErr = index.Lookup(key)
	} else {
		if request.Effect == "" {
			request.Effect = "none"
		}
		entry, found, lookupErr = index.LookupAlias(request.CatalogHash, request.RequestedQuality, request.Effect)
	}
	if lookupErr != nil {
		return model.CacheLookup{}, lookupErr
	}
	if !found {
		return model.CacheLookup{Status: "miss"}, nil
	}
	status := "partial"
	path := ""
	if entry.Complete {
		status = "complete"
		path = entry.RelativePath
	}
	return model.CacheLookup{
		Status: status,
		Key:    entry.Track.Key,
		Track:  entry.Track,
		Path:   path,
		Size:   entry.Size,
		Ranges: append([]model.ByteRange(nil), entry.Ranges...),
	}, nil
}

func (manager *Manager) availableIndex() (*cache.Index, error) {
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, fmt.Errorf("proxy manager is closed")
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache manager unavailable: %w", err)
		}
		if manager.migrating {
			done := manager.migrationDone
			manager.mu.Unlock()
			<-done
			continue
		}
		index := manager.index
		manager.mu.Unlock()
		return index, nil
	}
}

func (manager *Manager) UpdateURL(key, remoteURL string) error {
	validatedKey, err := validateCacheKey(key)
	if err != nil {
		return err
	}
	parsedURL, err := security.ValidateRemoteURL(remoteURL)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	session := manager.sessions[validatedKey]
	closed := manager.closed
	unavailable := manager.unavailableErr
	manager.mu.Unlock()
	if closed {
		return fmt.Errorf("proxy manager is closed")
	}
	if unavailable != nil {
		return fmt.Errorf("cache manager unavailable: %w", unavailable)
	}
	if session == nil {
		return fmt.Errorf("cache session %q does not exist", validatedKey)
	}
	return session.updateURL(parsedURL.String())
}

func (manager *Manager) Cancel(taskID string) error {
	for {
		manager.mu.Lock()
		if manager.migrating {
			done := manager.migrationDone
			manager.mu.Unlock()
			<-done
			continue
		}
		manager.mu.Unlock()
		break
	}
	manager.mu.Lock()
	if manager.migrating {
		done := manager.migrationDone
		manager.mu.Unlock()
		<-done
		return manager.Cancel(taskID)
	}
	if download := manager.downloads[taskID]; download != nil {
		manager.mu.Unlock()
		return download.cancelTask()
	}
	var found *Session
	var key string
	for sessionKey, session := range manager.sessions {
		if session.ID() == taskID {
			found = session
			key = sessionKey
			break
		}
	}
	archived, wasArchived := manager.history[taskID]
	if found == nil {
		manager.mu.Unlock()
		if wasArchived && archived.State == model.TaskCanceled {
			return nil
		}
		return fmt.Errorf("task %q does not exist", taskID)
	}
	claim, err := found.claimCancellation()
	manager.references[key]++
	manager.condition.Broadcast()
	manager.mu.Unlock()
	defer manager.releaseReference(key, found, referenceProducer)
	if err != nil {
		return err
	}
	shutdownErr := found.completeShutdown(claim)
	manager.mu.Lock()
	if key != "" && manager.sessions[key] == found {
		manager.archiveCanceledLocked(key, found)
	}
	manager.mu.Unlock()
	return shutdownErr
}

func (manager *Manager) Tasks() []model.Task {
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	history := make([]model.Task, 0, len(manager.history))
	for _, task := range manager.history {
		history = append(history, task)
	}
	downloads := make([]*downloadTask, 0, len(manager.downloads))
	for _, download := range manager.downloads {
		downloads = append(downloads, download)
	}
	manager.mu.Unlock()
	byID := make(map[string]model.Task, len(sessions)+len(history)+len(downloads))
	for _, task := range history {
		byID[task.ID] = task
	}
	for _, session := range sessions {
		task := session.task()
		byID[task.ID] = task
	}
	for _, download := range downloads {
		task := download.snapshot()
		byID[task.ID] = task
	}
	tasks := make([]model.Task, 0, len(byID))
	for _, task := range byID {
		tasks = append(tasks, task)
	}
	sortTasks(tasks)
	return tasks
}

// ProtectedKeys returns a point-in-time set for cache LRU operations. Retained
// completed session metadata alone does not protect its cache file.
func (manager *Manager) ProtectedKeys() map[string]bool {
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	protected := make(map[string]bool, len(manager.references))
	for key, count := range manager.references {
		if count > 0 {
			protected[key] = true
		}
	}
	manager.mu.Unlock()
	for _, session := range sessions {
		if session.protected() {
			protected[session.Key()] = true
		}
	}
	return protected
}

// Session returns a playback session only when both its ID and token match.
func (manager *Manager) Session(id, token string) (*Session, bool) {
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.Unlock()
	for _, session := range sessions {
		if session.ID() == id && session.authorized(token) {
			return session, true
		}
	}
	return nil, false
}

func (manager *Manager) Close() error {
	manager.closeOnce.Do(func() {
		manager.mu.Lock()
		manager.closed = true
		downloads := make([]*downloadTask, 0, len(manager.downloads))
		for _, download := range manager.downloads {
			downloads = append(downloads, download)
		}
		manager.condition.Broadcast()
		manager.mu.Unlock()
		for _, download := range downloads {
			manager.closeErr = errors.Join(manager.closeErr, download.close())
		}

		manager.mu.Lock()
		for manager.migrating || manager.cleaning {
			manager.condition.Wait()
		}
		sessions := make([]*Session, 0, len(manager.sessions))
		for _, session := range manager.sessions {
			sessions = append(sessions, session)
		}
		manager.mu.Unlock()
		for _, session := range sessions {
			manager.closeErr = errors.Join(manager.closeErr, session.close(true))
		}

		manager.mu.Lock()
		index := manager.index
		manager.index = nil
		manager.mu.Unlock()
		if index != nil {
			manager.closeErr = errors.Join(manager.closeErr, index.Close())
		}
	})
	return manager.closeErr
}

func (manager *Manager) archiveCanceledLocked(key string, session *Session) {
	if manager.sessions[key] == session {
		delete(manager.sessions, key)
	}
	id := session.ID()
	manager.history[id] = session.task()
	for _, existing := range manager.historyOrder {
		if existing == id {
			return
		}
	}
	manager.historyOrder = append(manager.historyOrder, id)
	if len(manager.historyOrder) > maxCanceledTaskHistory {
		oldest := manager.historyOrder[0]
		manager.historyOrder = manager.historyOrder[1:]
		delete(manager.history, oldest)
	}
}

func (manager *Manager) expireSession(key string, expected *Session) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.sessions[key] != expected || !expected.canExpire() {
		return
	}
	delete(manager.sessions, key)
}

type referenceKind uint8

const (
	referenceReader referenceKind = iota
	referenceProducer
	referenceExport
)

func (manager *Manager) blockedDoneLocked() <-chan struct{} {
	if manager.migrating {
		return manager.migrationDone
	}
	if manager.cleaning {
		return manager.cleaningDone
	}
	return nil
}

func (manager *Manager) acquireReference(key string, session *Session, kind referenceKind) (func(), error) {
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, fmt.Errorf("proxy manager is closed")
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache manager unavailable: %w", err)
		}
		if blocked := manager.blockedDoneLocked(); blocked != nil {
			manager.mu.Unlock()
			<-blocked
			continue
		}
		manager.references[key]++
		if session == nil && kind == referenceExport {
			session = manager.sessions[key]
		}
		if session != nil && kind == referenceExport {
			session.mu.Lock()
			session.exports++
			session.mu.Unlock()
		}
		manager.condition.Broadcast()
		manager.mu.Unlock()
		break
	}

	var once sync.Once
	return func() {
		once.Do(func() { manager.releaseReference(key, session, kind) })
	}, nil
}

func (manager *Manager) releaseReference(key string, session *Session, kind referenceKind) {
	manager.mu.Lock()
	if session != nil && kind == referenceExport {
		session.mu.Lock()
		if session.exports > 0 {
			session.exports--
		}
		session.mu.Unlock()
	}
	if manager.references[key] > 1 {
		manager.references[key]--
	} else {
		delete(manager.references, key)
	}
	last := manager.references[key] == 0
	closed := manager.closed
	migrating := manager.migrating
	manager.condition.Broadcast()
	manager.mu.Unlock()
	if session != nil && kind == referenceExport {
		manager.expireSession(key, session)
	}
	if last && !closed && !migrating {
		_ = manager.cleanCache()
	}
}

func (manager *Manager) SetLimit(bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("cache limit must not be negative")
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return fmt.Errorf("proxy manager is closed")
	}
	if manager.unavailableErr != nil {
		err := manager.unavailableErr
		manager.mu.Unlock()
		return fmt.Errorf("cache manager unavailable: %w", err)
	}
	manager.cacheLimit = bytes
	manager.mu.Unlock()
	return manager.cleanCache()
}

func (manager *Manager) cleanCache() error {
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return fmt.Errorf("cache manager unavailable: %w", err)
		}
		if blocked := manager.blockedDoneLocked(); blocked != nil {
			manager.mu.Unlock()
			<-blocked
			continue
		}
		manager.cleaning = true
		manager.cleaningDone = make(chan struct{})
		index := manager.index
		limit := manager.cacheLimit
		protected := make(map[string]bool, len(manager.references))
		for key, count := range manager.references {
			if count > 0 {
				protected[key] = true
			}
		}
		manager.mu.Unlock()

		var err error
		if index == nil {
			err = fmt.Errorf("cache index is required")
		} else {
			_, err = index.CleanTo(limit, protected)
		}

		manager.mu.Lock()
		manager.cleaning = false
		close(manager.cleaningDone)
		manager.cleaningDone = nil
		manager.condition.Broadcast()
		manager.mu.Unlock()
		return err
	}
}

func normalizeTrack(track model.TrackInfo) (string, model.TrackInfo, error) {
	if track.Effect == "" {
		track.Effect = "none"
	}
	key, err := model.CacheKey(track.Hash, track.Quality, track.Effect)
	if err != nil {
		return "", model.TrackInfo{}, err
	}
	parts := strings.Split(key, ".")
	track.Key = key
	track.Hash = parts[0]
	track.Quality = parts[1]
	track.Effect = parts[2]
	originalExtension := track.Extension
	track.Extension = normalizeExtension(originalExtension)
	if strings.TrimSpace(originalExtension) != "" && track.Extension == "" {
		return "", model.TrackInfo{}, fmt.Errorf("invalid audio extension")
	}
	if track.Extension == "" {
		track.Extension = "mp3"
	}
	if track.CatalogHash != "" || track.RequestedQuality != "" {
		aliasKey, aliasErr := model.CacheKey(track.CatalogHash, track.RequestedQuality, track.Effect)
		if aliasErr != nil {
			return "", model.TrackInfo{}, fmt.Errorf("invalid cache alias: %w", aliasErr)
		}
		track.CatalogHash = strings.Split(aliasKey, ".")[0]
	}
	return key, track, nil
}

func validateCacheKey(key string) (string, error) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid cache key")
	}
	validated, err := model.CacheKey(parts[0], parts[1], parts[2])
	if err != nil || validated != key {
		return "", fmt.Errorf("invalid cache key")
	}
	return validated, nil
}

func normalizeExtension(extension string) string {
	extension = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(extension), "."))
	if extension == "" || len(extension) > 16 {
		return ""
	}
	for _, character := range extension {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return ""
		}
	}
	return extension
}

func mergeStoredTrack(stored, requested model.TrackInfo) model.TrackInfo {
	requested.Key = stored.Key
	requested.Hash = stored.Hash
	requested.Quality = stored.Quality
	requested.Effect = stored.Effect
	if stored.Extension != "" {
		requested.Extension = stored.Extension
	}
	if requested.CatalogHash == "" {
		requested.CatalogHash = stored.CatalogHash
	}
	if requested.RequestedQuality == "" {
		requested.RequestedQuality = stored.RequestedQuality
	}
	return requested
}

func newSecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func secretsEqual(left, right string) bool {
	if len(left) != len(right) || left == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
