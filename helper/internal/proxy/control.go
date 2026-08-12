package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

var ErrCacheEntryActive = errors.New("cache entry is active")

type CacheItem struct {
	Key            string            `json:"key"`
	Track          model.TrackInfo   `json:"track"`
	Status         string            `json:"status"`
	Size           int64             `json:"size"`
	TotalBytes     int64             `json:"totalBytes"`
	Ranges         []model.ByteRange `json:"ranges,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
	CompletedAt    time.Time         `json:"completedAt,omitempty"`
	LastAccessedAt time.Time         `json:"lastAccessedAt"`
}

type CacheView struct {
	Items           []CacheItem `json:"items"`
	CacheBytes      int64       `json:"cacheBytes"`
	CacheLimitBytes int64       `json:"cacheLimitBytes"`
}

func (manager *Manager) CacheView() (CacheView, error) {
	index, _, finish, err := manager.beginCacheControl()
	if err != nil {
		return CacheView{}, err
	}
	defer finish()
	manager.mu.Lock()
	limit := manager.cacheLimit
	manager.mu.Unlock()
	entries := index.Snapshot()
	view := CacheView{Items: make([]CacheItem, 0, len(entries)), CacheLimitBytes: limit}
	for key, entry := range entries {
		status := "partial"
		if entry.Complete {
			status = "complete"
		}
		view.CacheBytes += entry.Size
		view.Items = append(view.Items, CacheItem{
			Key: key, Track: entry.Track, Status: status, Size: entry.Size,
			TotalBytes: entry.TotalBytes, Ranges: append([]model.ByteRange(nil), entry.Ranges...),
			CreatedAt: entry.CreatedAt, CompletedAt: entry.CompletedAt, LastAccessedAt: entry.LastAccessedAt,
		})
	}
	sort.Slice(view.Items, func(i, j int) bool {
		if view.Items[i].LastAccessedAt.Equal(view.Items[j].LastAccessedAt) {
			return view.Items[i].Key < view.Items[j].Key
		}
		return view.Items[i].LastAccessedAt.Before(view.Items[j].LastAccessedAt)
	})
	return view, nil
}

func (manager *Manager) CacheRoot() (string, error) {
	index, _, finish, err := manager.beginCacheControl()
	if err != nil {
		return "", err
	}
	defer finish()
	return index.Root(), nil
}

// CompleteCachePath resolves a complete indexed entry to a validated absolute
// local path for authenticated protocol consumers. It does not grant reveal or
// deletion authority over that path.
func (manager *Manager) CompleteCachePath(key string) (string, bool, error) {
	validated, err := validateCacheKey(key)
	if err != nil {
		return "", false, err
	}
	index, _, finish, err := manager.beginCacheControl()
	if err != nil {
		return "", false, err
	}
	defer finish()
	entry, found, err := index.Lookup(validated)
	if err != nil {
		return "", false, err
	}
	if !found || !entry.Complete {
		return "", false, nil
	}
	if manager.afterCompleteLookup != nil {
		manager.afterCompleteLookup()
	}
	statCacheEntry := manager.statCacheEntry
	if statCacheEntry == nil {
		statCacheEntry = func(index *cache.Index, relativePath string) (os.FileInfo, error) {
			return index.StatFile(relativePath)
		}
	}
	info, err := statCacheEntry(index, entry.RelativePath)
	if err != nil {
		if cache.IsStaleFileError(err) {
			_ = index.Remove(validated)
			return "", false, nil
		}
		return "", false, fmt.Errorf("validate complete cache file: %w", err)
	}
	if info.Size() != entry.Size {
		_ = index.Remove(validated)
		return "", false, nil
	}
	if _, err := index.RootIdentity(); err != nil {
		return "", false, fmt.Errorf("validate current cache root: %w", err)
	}
	root := index.Root()
	path := filepath.Join(root, filepath.FromSlash(entry.RelativePath))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || !filepath.IsLocal(relative) || !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("validated cache entry did not resolve beneath the current absolute root")
	}
	now := manager.controlNow
	if now == nil {
		now = time.Now
	}
	touchCacheEntry := manager.touchCacheEntry
	if touchCacheEntry == nil {
		touchCacheEntry = func(index *cache.Index, key string, at time.Time) error {
			return index.Touch(key, at)
		}
	}
	if err := touchCacheEntry(index, validated, now().UTC()); err != nil {
		return "", false, fmt.Errorf("persist complete cache access: %w", err)
	}
	return path, true, nil
}

func (manager *Manager) DeleteCache(key string) (bool, error) {
	validated, err := validateCacheKey(key)
	if err != nil {
		return false, err
	}
	index, protected, finish, err := manager.beginCacheControl()
	if err != nil {
		return false, err
	}
	defer finish()
	if protected[validated] {
		return false, ErrCacheEntryActive
	}
	return index.DeleteControlled(validated)
}

func (manager *Manager) ClearCache() (cache.CleanupResult, error) {
	index, protected, finish, err := manager.beginCacheControl()
	if err != nil {
		return cache.CleanupResult{}, err
	}
	defer finish()
	return index.ClearControlled(protected)
}

func (manager *Manager) beginCacheControl() (*cache.Index, map[string]bool, func(), error) {
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, nil, nil, fmt.Errorf("proxy manager is closed")
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return nil, nil, nil, fmt.Errorf("cache manager unavailable: %w", err)
		}
		if blocked := manager.blockedDoneLocked(); blocked != nil {
			manager.mu.Unlock()
			<-blocked
			continue
		}
		if manager.index == nil {
			manager.mu.Unlock()
			return nil, nil, nil, fmt.Errorf("cache index is required")
		}
		manager.cleaning = true
		manager.cleaningDone = make(chan struct{})
		index := manager.index
		protected := make(map[string]bool, len(manager.references))
		for key, count := range manager.references {
			if count > 0 {
				protected[key] = true
			}
		}
		sessions := make([]*Session, 0, len(manager.sessions))
		for _, session := range manager.sessions {
			sessions = append(sessions, session)
		}
		manager.mu.Unlock()
		for _, session := range sessions {
			if session.protected() {
				protected[session.Key()] = true
			}
		}
		var once bool
		finish := func() {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			if once {
				return
			}
			once = true
			manager.cleaning = false
			close(manager.cleaningDone)
			manager.cleaningDone = nil
			manager.condition.Broadcast()
		}
		return index, protected, finish, nil
	}
}

// RevealTarget validates an exact user-visible target without granting broad
// access to arbitrary descendants of a managed directory.
func (manager *Manager) RevealTarget(kind, requested string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(requested))
	if err != nil || requested == "" {
		return "", fmt.Errorf("reveal path is required")
	}
	index, _, finish, err := manager.beginCacheControl()
	if err != nil {
		return "", err
	}
	defer finish()
	manager.mu.Lock()
	roots := append([]string(nil), manager.downloadRoots...)
	records := make([]string, 0, len(manager.downloadRecords))
	for _, record := range manager.downloadRecords {
		records = append(records, record)
	}
	rootInfo := make(map[string]os.FileInfo, len(manager.downloadRootInfo))
	for key, info := range manager.downloadRootInfo {
		rootInfo[key] = info
	}
	manager.mu.Unlock()
	if kind == "open-directory" {
		if samePath(absolute, index.Root()) {
			if _, err := index.RootIdentity(); err != nil {
				return "", err
			}
			return index.Root(), nil
		}
		for _, root := range roots {
			if !samePath(absolute, root) {
				continue
			}
			directory, err := security.OpenDirectory(root, false)
			if err != nil {
				return "", err
			}
			valid := rootInfo[downloadPathKey(root)] != nil && os.SameFile(rootInfo[downloadPathKey(root)], directory.Identity())
			closeErr := directory.Close()
			if !valid {
				return "", fmt.Errorf("managed download root identity changed")
			}
			return root, closeErr
		}
		return "", fmt.Errorf("directory is not an exact managed root")
	}
	if kind != "select" {
		return "", fmt.Errorf("unsupported reveal kind")
	}
	for _, entry := range index.Snapshot() {
		candidate := filepath.Join(index.Root(), filepath.FromSlash(entry.RelativePath))
		if !samePath(absolute, candidate) {
			continue
		}
		if _, err := index.StatFile(entry.RelativePath); err != nil {
			return "", err
		}
		return candidate, nil
	}
	for _, record := range records {
		if !samePath(absolute, record) {
			continue
		}
		rootPath, relative, ok := downloadPathWithinRoots(record, roots)
		if !ok {
			return "", fmt.Errorf("recorded download is outside managed roots")
		}
		directory, err := security.OpenDirectory(rootPath, false)
		if err != nil {
			return "", err
		}
		info, statErr := directory.Lstat(relative)
		valid := statErr == nil && info.Mode().IsRegular() && rootInfo[downloadPathKey(rootPath)] != nil && os.SameFile(rootInfo[downloadPathKey(rootPath)], directory.Identity())
		closeErr := directory.Close()
		if !valid {
			return "", errors.Join(fmt.Errorf("recorded download is not a valid regular file"), statErr, closeErr)
		}
		return record, closeErr
	}
	return "", fmt.Errorf("path is not an exact managed cache or download target")
}
