package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

const (
	indexFilename     = "index.json"
	indexTempFilename = "index.json.tmp"
)

var errInvalidControlledFile = errors.New("invalid controlled cache file")

// IsStaleFileError reports whether a controlled cache file disappeared or no
// longer names a regular file. Other filesystem and root validation failures
// must remain visible to callers.
func IsStaleFileError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, errInvalidControlledFile)
}

type indexHooks struct {
	statFile   func(string) (os.FileInfo, error)
	persist    func(map[string]Entry) error
	removeFile func(string) error
	scanAudio  func() (map[string]Entry, error)
}

type Entry struct {
	Track          model.TrackInfo   `json:"track"`
	AliasKey       string            `json:"aliasKey"`
	RelativePath   string            `json:"relativePath"`
	Size           int64             `json:"size"`
	TotalBytes     int64             `json:"totalBytes"`
	Ranges         []model.ByteRange `json:"ranges"`
	Complete       bool              `json:"complete"`
	CreatedAt      time.Time         `json:"createdAt"`
	CompletedAt    time.Time         `json:"completedAt,omitempty"`
	LastAccessedAt time.Time         `json:"lastAccessedAt"`
}

type CleanupResult struct {
	Deleted     []string `json:"deleted"`
	BeforeBytes int64    `json:"beforeBytes"`
	AfterBytes  int64    `json:"afterBytes"`
}

type Index struct {
	mu        sync.RWMutex
	root      string
	indexPath string
	rootFS    *os.Root
	rootLock  rootPathLock
	audioFS   *os.Root
	tempFS    *os.Root
	audioInfo os.FileInfo
	tempInfo  os.FileInfo
	entries   map[string]Entry
	hooks     *indexHooks
}

func Open(root string) (*Index, error) {
	return openWithHooks(root, nil)
}

// OpenExisting opens an existing cache root without creating any path
// component. It is used when recovery must not turn a missing candidate into a
// new, empty cache.
func OpenExisting(root string) (*Index, error) {
	return openIndexRoot(root, nil, false)
}

// OpenAnchored initializes a cache index through an already-opened root
// capability. It takes ownership of rootFS, including on error.
func OpenAnchored(root string, rootFS *os.Root) (*Index, error) {
	if rootFS == nil {
		return nil, fmt.Errorf("opened cache root is required")
	}
	rootPath, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		_ = rootFS.Close()
		return nil, fmt.Errorf("resolve cache root: %w", err)
	}
	return openIndexCapability(rootPath, rootFS, nil, true)
}

func openWithHooks(root string, hooks *indexHooks) (*Index, error) {
	return openIndexRoot(root, hooks, true)
}

func openIndexRoot(root string, hooks *indexHooks, create bool) (*Index, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("cache root is required")
	}
	var rootFS *os.Root
	var rootPath string
	var err error
	if create {
		rootFS, rootPath, err = openOrCreateRoot(root)
	} else {
		rootFS, rootPath, err = openExistingRoot(root)
	}
	if err != nil {
		return nil, err
	}
	return openIndexCapability(rootPath, rootFS, hooks, create)
}

func openIndexCapability(rootPath string, rootFS *os.Root, hooks *indexHooks, create bool) (*Index, error) {
	rootLock, err := lockRootPath(rootPath)
	if err != nil {
		_ = rootFS.Close()
		return nil, fmt.Errorf("lock cache root path: %w", err)
	}
	if err := verifyRootPath(rootFS, rootPath); err != nil {
		_ = rootLock.Close()
		_ = rootFS.Close()
		return nil, err
	}
	index := &Index{
		root:      rootPath,
		indexPath: filepath.Join(rootPath, indexFilename),
		rootFS:    rootFS,
		rootLock:  rootLock,
		entries:   make(map[string]Entry),
		hooks:     hooks,
	}
	if err := index.openCacheDirectories(create); err != nil {
		_ = index.closeRoots()
		return nil, err
	}
	if create {
		err = index.load()
	} else {
		err = index.loadExisting()
	}
	if err != nil {
		_ = index.closeRoots()
		return nil, err
	}
	return index, nil
}

func (index *Index) Close() error {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.closeRoots()
}

func (index *Index) Lookup(key string) (Entry, bool, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	entry, ok := index.entries[key]
	if !ok {
		return Entry{}, false, nil
	}
	stale, err := index.validateStoredFile(entry)
	if err != nil {
		return Entry{}, false, fmt.Errorf("validate cache entry %q: %w", key, err)
	}
	if stale {
		if err := index.removeStaleLocked(key); err != nil {
			return Entry{}, false, fmt.Errorf("remove stale cache entry %q: %w", key, err)
		}
		return Entry{}, false, nil
	}
	return cloneEntry(entry), true, nil
}

func (index *Index) LookupAlias(catalogHash, requestedQuality, effect string) (Entry, bool, error) {
	aliasKey, err := normalizedAliasKey(catalogHash, requestedQuality, effect)
	if err != nil {
		return Entry{}, false, err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	keys := sortedKeys(index.entries)
	for _, key := range keys {
		entry := index.entries[key]
		if !entry.Complete || entry.AliasKey != aliasKey {
			continue
		}
		stale, err := index.validateStoredFile(entry)
		if err != nil {
			return Entry{}, false, fmt.Errorf("validate cache alias entry %q: %w", key, err)
		}
		if stale {
			if err := index.removeStaleLocked(key); err != nil {
				return Entry{}, false, fmt.Errorf("remove stale cache alias entry %q: %w", key, err)
			}
			continue
		}
		return cloneEntry(entry), true, nil
	}
	return Entry{}, false, nil
}

func (index *Index) Upsert(key string, entry Entry) error {
	entry, err := normalizeEntry(key, entry)
	if err != nil {
		return err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	next := cloneEntries(index.entries)
	next[key] = cloneEntry(entry)
	if err := index.persistLocked(next); err != nil {
		return err
	}
	index.entries = next
	return nil
}

func (index *Index) Remove(key string) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	if _, ok := index.entries[key]; !ok {
		return nil
	}
	next := cloneEntries(index.entries)
	delete(next, key)
	if err := index.persistLocked(next); err != nil {
		return err
	}
	index.entries = next
	return nil
}

func (index *Index) Touch(key string, at time.Time) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	entry, ok := index.entries[key]
	if !ok {
		return fmt.Errorf("cache entry %q does not exist", key)
	}
	entry.LastAccessedAt = at
	next := cloneEntries(index.entries)
	next[key] = cloneEntry(entry)
	if err := index.persistLocked(next); err != nil {
		return err
	}
	index.entries = next
	return nil
}

func (index *Index) Snapshot() map[string]Entry {
	index.mu.RLock()
	defer index.mu.RUnlock()
	return cloneEntries(index.entries)
}

// Root returns the validated cache root selected when the index was opened.
func (index *Index) Root() string {
	index.mu.RLock()
	defer index.mu.RUnlock()
	return index.root
}

// RootIdentity returns the identity of the cache root held by the index after
// confirming that the configured path still names that same directory.
func (index *Index) RootIdentity() (os.FileInfo, error) {
	index.mu.RLock()
	defer index.mu.RUnlock()
	if index.rootFS == nil {
		return nil, fmt.Errorf("cache index is closed")
	}
	if err := verifyRootPath(index.rootFS, index.root); err != nil {
		return nil, err
	}
	opened, err := index.rootFS.Open(".")
	if err != nil {
		return nil, fmt.Errorf("inspect opened cache root: %w", err)
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened cache root: %w", err)
	}
	return info, nil
}

// CopyTo copies controlled cache files and their metadata through the rooted
// APIs of both indexes. The destination must be empty.
func (index *Index) CopyTo(destination *Index) error {
	if destination == nil {
		return fmt.Errorf("destination cache index is required")
	}
	if filepath.Clean(index.Root()) == filepath.Clean(destination.Root()) {
		return fmt.Errorf("source and destination cache roots must differ")
	}
	if len(destination.Snapshot()) != 0 {
		return fmt.Errorf("destination cache index is not empty")
	}
	entries := index.Snapshot()
	for _, key := range sortedKeys(entries) {
		entry := entries[key]
		input, err := index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open source cache entry %q: %w", key, err)
		}
		info, statErr := input.Stat()
		if statErr != nil {
			_ = input.Close()
			return fmt.Errorf("inspect source cache entry %q: %w", key, statErr)
		}
		output, err := destination.OpenFile(entry.RelativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			return fmt.Errorf("create destination cache entry %q: %w", key, err)
		}
		written, copyErr := io.CopyBuffer(output, input, make([]byte, 64*1024))
		syncErr := output.Sync()
		closeErr := errors.Join(output.Close(), input.Close())
		if operationErr := errors.Join(copyErr, syncErr, closeErr); operationErr != nil {
			return fmt.Errorf("copy cache entry %q: %w", key, operationErr)
		}
		if written != info.Size() {
			return fmt.Errorf("copy cache entry %q: copied %d of %d bytes", key, written, info.Size())
		}
		if err := destination.Upsert(key, entry); err != nil {
			return fmt.Errorf("persist destination cache entry %q: %w", key, err)
		}
	}
	return nil
}

// OpenFile safely opens a validated cache path relative to the anchored cache root.
func (index *Index) OpenFile(relativePath string, flag int, perm os.FileMode) (*os.File, error) {
	directory, basename, err := parseCacheRelativePath(relativePath)
	if err != nil {
		return nil, err
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	root, err := index.subrootLocked(directory)
	if err != nil {
		return nil, err
	}
	before, beforeErr := root.Lstat(basename)
	if beforeErr == nil && !before.Mode().IsRegular() {
		return nil, fmt.Errorf("cache path %q is not a regular file", relativePath)
	}
	if beforeErr != nil && !errors.Is(beforeErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect cache path %q: %w", relativePath, beforeErr)
	}
	truncate := flag&os.O_TRUNC != 0
	file, err := openRootFile(root, basename, flag&^os.O_TRUNC, perm)
	if err != nil {
		return nil, fmt.Errorf("open cache path %q: %w", relativePath, err)
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect opened cache path %q: %w", relativePath, err)
		}
		return nil, fmt.Errorf("cache path %q is not a regular file", relativePath)
	}
	current, err := root.Lstat(basename)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("cache path %q changed while it was opened", relativePath)
	}
	if truncate {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("truncate cache path %q: %w", relativePath, err)
		}
	}
	return file, nil
}

func (index *Index) StatFile(relativePath string) (os.FileInfo, error) {
	directory, basename, err := parseCacheRelativePath(relativePath)
	if err != nil {
		return nil, err
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	root, err := index.subrootLocked(directory)
	if err != nil {
		return nil, err
	}
	info, err := root.Lstat(basename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: cache path %q is not a regular file", errInvalidControlledFile, relativePath)
	}
	return info, nil
}

func (index *Index) RemoveFile(relativePath string) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.removeFileLocked(relativePath)
}

func (index *Index) RenameFile(oldPath, newPath string) error {
	oldDirectory, oldBasename, err := parseCacheRelativePath(oldPath)
	if err != nil {
		return err
	}
	newDirectory, newBasename, err := parseCacheRelativePath(newPath)
	if err != nil {
		return err
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	oldRoot, err := index.subrootLocked(oldDirectory)
	if err != nil {
		return err
	}
	oldInfo, err := oldRoot.Lstat(oldBasename)
	if err != nil {
		return fmt.Errorf("inspect source cache path %q: %w", oldPath, err)
	}
	if !oldInfo.Mode().IsRegular() {
		return fmt.Errorf("source cache path %q is not a regular file", oldPath)
	}
	newRoot, err := index.subrootLocked(newDirectory)
	if err != nil {
		return err
	}
	if newInfo, statErr := newRoot.Lstat(newBasename); statErr == nil && !newInfo.Mode().IsRegular() {
		return fmt.Errorf("destination cache path %q is not a regular file", newPath)
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect destination cache path %q: %w", newPath, statErr)
	}
	if err := index.rootFS.Rename(path.Join(oldDirectory, oldBasename), path.Join(newDirectory, newBasename)); err != nil {
		return fmt.Errorf("rename cache path %q to %q: %w", oldPath, newPath, err)
	}
	renamedInfo, err := newRoot.Lstat(newBasename)
	if err != nil || !renamedInfo.Mode().IsRegular() || !os.SameFile(oldInfo, renamedInfo) {
		return fmt.Errorf("cache path %q changed while it was renamed", oldPath)
	}
	return nil
}

func (index *Index) openCacheDirectories(create bool) error {
	for _, directory := range []string{"audio", "temp"} {
		subroot, info, err := security.OpenChildDirectory(index.rootFS, directory, create)
		if err != nil {
			return fmt.Errorf("open or create cache directory %q: %w", directory, err)
		}
		if directory == "audio" {
			index.audioFS = subroot
			index.audioInfo = info
		} else {
			index.tempFS = subroot
			index.tempInfo = info
		}
	}
	return nil
}

func (index *Index) validateStoredFile(entry Entry) (bool, error) {
	var info os.FileInfo
	var err error
	if index.hooks != nil && index.hooks.statFile != nil {
		info, err = index.hooks.statFile(entry.RelativePath)
	} else {
		info, err = index.statFileLocked(entry.RelativePath)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errInvalidControlledFile) {
			return true, nil
		}
		return false, err
	}
	if entry.Complete && info.Size() != entry.Size {
		return true, nil
	}
	return false, nil
}

func (index *Index) statFileLocked(relativePath string) (os.FileInfo, error) {
	directory, basename, err := parseCacheRelativePath(relativePath)
	if err != nil {
		return nil, err
	}
	root, err := index.subrootLocked(directory)
	if err != nil {
		return nil, err
	}
	info, err := root.Lstat(basename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: cache path %q is not a regular file", errInvalidControlledFile, relativePath)
	}
	return info, nil
}

func (index *Index) removeFileLocked(relativePath string) error {
	if index.hooks != nil && index.hooks.removeFile != nil {
		return index.hooks.removeFile(relativePath)
	}
	directory, basename, err := parseCacheRelativePath(relativePath)
	if err != nil {
		return err
	}
	root, err := index.subrootLocked(directory)
	if err != nil {
		return err
	}
	info, err := root.Lstat(basename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cache path %q: %w", relativePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cache path %q is not a regular file", relativePath)
	}
	if err := root.Remove(basename); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove cache path %q: %w", relativePath, err)
	}
	return nil
}

func (index *Index) subrootLocked(directory string) (*os.Root, error) {
	if index.rootFS == nil {
		return nil, fmt.Errorf("cache index is closed")
	}
	var expected os.FileInfo
	var root *os.Root
	switch directory {
	case "audio":
		root, expected = index.audioFS, index.audioInfo
	case "temp":
		root, expected = index.tempFS, index.tempInfo
	default:
		return nil, fmt.Errorf("unsupported cache directory %q", directory)
	}
	if root == nil || expected == nil {
		return nil, fmt.Errorf("cache index is closed")
	}
	current, err := index.rootFS.Lstat(directory)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, expected) {
		return nil, fmt.Errorf("cache directory %q changed after it was opened", directory)
	}
	return root, nil
}

func (index *Index) removeStaleLocked(key string) error {
	next := cloneEntries(index.entries)
	delete(next, key)
	if err := index.persistLocked(next); err != nil {
		return err
	}
	index.entries = next
	return nil
}

func (index *Index) closeRoots() error {
	var closeError error
	for _, root := range []*os.Root{index.audioFS, index.tempFS, index.rootFS} {
		if root != nil {
			closeError = errors.Join(closeError, root.Close())
		}
	}
	if index.rootLock != nil {
		closeError = errors.Join(closeError, index.rootLock.Close())
	}
	index.audioFS = nil
	index.tempFS = nil
	index.rootFS = nil
	index.rootLock = nil
	index.audioInfo = nil
	index.tempInfo = nil
	return closeError
}
