package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

const (
	testHash        = "0123456789abcdef"
	testCatalogHash = "fedcba9876543210"
)

func TestIndexPersistsLookupAliasTouchAndRemove(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 8)

	index := openIndex(t, root)
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	entry := completeEntry(key, testCatalogHash, "320", "mp3", 8, now)
	if err := index.Upsert(key, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "index.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary index remains after atomic write: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	index = openIndex(t, root)
	defer index.Close()
	got, ok, _ := index.Lookup(key)
	if !ok || !reflect.DeepEqual(got, entry) {
		t.Fatalf("lookup: got %#v, %v want %#v", got, ok, entry)
	}
	aliased, ok, _ := index.LookupAlias(strings.ToUpper(testCatalogHash), "320", "none")
	if !ok || aliased.Track.Key != key {
		t.Fatalf("alias lookup: got %#v, %v", aliased, ok)
	}

	touchedAt := now.Add(time.Hour)
	if err := index.Touch(key, touchedAt); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := index.Lookup(key); !got.LastAccessedAt.Equal(touchedAt) {
		t.Fatalf("touch time: got %v want %v", got.LastAccessedAt, touchedAt)
	}
	snapshot := index.Snapshot()
	snapshot[key] = Entry{}
	if snapshotAgain := index.Snapshot(); snapshotAgain[key].Track.Key != key {
		t.Fatal("Snapshot returned the index's mutable map")
	}
	if err := index.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("removed entry remained in index")
	}
}

func TestLookupRemovesMissingEntryPersistently(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	index := openIndex(t, root)
	entry := completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())
	if err := index.Upsert(key, entry); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("missing cache file produced a lookup hit")
	}
	if _, ok := index.Snapshot()[key]; ok {
		t.Fatal("missing cache file remained in memory")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	index = openIndex(t, root)
	defer index.Close()
	if _, ok := index.Snapshot()[key]; ok {
		t.Fatal("missing cache file removal was not persisted")
	}
}

func TestLookupRetainsEntryOnTransientValidationFailure(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 8)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())); err != nil {
		t.Fatal(err)
	}
	index.hooks = &indexHooks{
		statFile: func(string) (os.FileInfo, error) {
			return nil, fmt.Errorf("sharing violation: %w", fs.ErrPermission)
		},
	}
	if _, ok, err := index.Lookup(key); ok || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("transient validation lookup = found %t, error %v", ok, err)
	}
	if _, ok := index.Snapshot()[key]; !ok {
		t.Fatal("transient validation failure evicted the entry")
	}
}

func TestLookupAliasPropagatesTransientValidationFailure(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 8)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())); err != nil {
		t.Fatal(err)
	}
	index.hooks = &indexHooks{statFile: func(string) (os.FileInfo, error) {
		return nil, fmt.Errorf("sharing violation: %w", fs.ErrPermission)
	}}
	if _, found, err := index.LookupAlias(testCatalogHash, "320", "none"); found || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("transient alias lookup = found %t, error %v", found, err)
	}
	if _, ok := index.Snapshot()[key]; !ok {
		t.Fatal("transient alias validation failure evicted the entry")
	}
}

func TestLookupRetainsStaleEntryWhenPersistenceFails(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 8)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "audio", key+".mp3")); err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("injected persistence failure")
	index.hooks = &indexHooks{persist: func(map[string]Entry) error { return persistErr }}
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("missing file produced a lookup hit")
	}
	if _, ok := index.Snapshot()[key]; !ok {
		t.Fatal("failed stale-entry persistence still mutated memory")
	}
	index.hooks = nil
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("missing file produced a lookup hit after fault removal")
	}
	if _, ok := index.Snapshot()[key]; ok {
		t.Fatal("stale entry remained after successful persistence")
	}
}

func TestOpenFailsWithoutFilteringOnTransientValidationFailure(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 8)
	index := openIndex(t, root)
	if err := index.Upsert(key, completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	hooks := &indexHooks{
		statFile: func(string) (os.FileInfo, error) {
			return nil, fmt.Errorf("injected I/O fault: %w", fs.ErrPermission)
		},
	}
	if reopened, err := openWithHooks(root, hooks); err == nil {
		_ = reopened.Close()
		t.Fatal("Open silently filtered a transient validation failure")
	}
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil || !strings.Contains(string(data), key) {
		t.Fatalf("startup transient fault changed the index: data=%q err=%v", data, err)
	}
}

func TestCorruptRecoveryRetriesAfterScanFailure(t *testing.T) {
	root, key := prepareCorruptRecovery(t)
	scanErr := errors.New("injected scan failure")
	if index, err := openWithHooks(root, &indexHooks{
		scanAudio: func() (map[string]Entry, error) { return nil, scanErr },
	}); !errors.Is(err, scanErr) {
		if index != nil {
			_ = index.Close()
		}
		t.Fatalf("Open error = %v, want scan failure", err)
	}
	assertCorruptIndexRestored(t, root)
	index := openIndex(t, root)
	defer index.Close()
	if _, ok, _ := index.Lookup(key); !ok {
		t.Fatal("later Open did not retry corrupt recovery")
	}
}

func TestCorruptRecoveryRetriesAfterPersistenceFailure(t *testing.T) {
	root, key := prepareCorruptRecovery(t)
	persistErr := errors.New("injected rebuild persistence failure")
	if index, err := openWithHooks(root, &indexHooks{
		persist: func(map[string]Entry) error { return persistErr },
	}); !errors.Is(err, persistErr) {
		if index != nil {
			_ = index.Close()
		}
		t.Fatalf("Open error = %v, want persistence failure", err)
	}
	assertCorruptIndexRestored(t, root)
	index := openIndex(t, root)
	defer index.Close()
	if _, ok, _ := index.Lookup(key); !ok {
		t.Fatal("later Open did not retry corrupt recovery")
	}
}

func prepareCorruptRecovery(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "flac")
	writeCacheFile(t, root, "audio/"+key+".flac", 17)
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte("{retry-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, key
}

func assertCorruptIndexRestored(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil || string(data) != "{retry-me" {
		t.Fatalf("corrupt index was not restored: data=%q err=%v", data, err)
	}
}

func TestOpenRejectsUnsafeStoredPaths(t *testing.T) {
	root := t.TempDir()
	escapePath := filepath.Join(filepath.Dir(root), "escape.mp3")
	if err := os.WriteFile(escapePath, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(escapePath) })

	key := mustCacheKey(t, testHash, "320")
	entry := completeEntry(key, testCatalogHash, "320", "mp3", 12, time.Now())
	entry.RelativePath = "../escape.mp3"
	writeIndexJSON(t, root, map[string]Entry{key: entry})

	index := openIndex(t, root)
	defer index.Close()
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("unsafe entry was loaded")
	}
	data, err := os.ReadFile(escapePath)
	if err != nil || string(data) != "do not touch" {
		t.Fatalf("external file was touched: data=%q err=%v", data, err)
	}
}

func TestOpenRejectsSymlinkedCacheFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires extra Windows privileges")
	}
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "external.mp3")
	if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := mustCacheKey(t, testHash, "320")
	if err := os.MkdirAll(filepath.Join(root, "audio"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "audio", key+".mp3")); err != nil {
		t.Fatal(err)
	}
	entry := completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())
	writeIndexJSON(t, root, map[string]Entry{key: entry})

	index := openIndex(t, root)
	defer index.Close()
	if _, ok, _ := index.Lookup(key); ok {
		t.Fatal("symlinked entry was loaded")
	}
	data, err := os.ReadFile(external)
	if err != nil || string(data) != "external" {
		t.Fatalf("external target was touched: data=%q err=%v", data, err)
	}
}

func TestOpenDoesNotCreateRootThroughSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires extra Windows privileges")
	}
	base := t.TempDir()
	external := t.TempDir()
	linkedParent := filepath.Join(base, "linked")
	if err := os.Symlink(external, linkedParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(linkedParent, "new-cache")
	if index, err := Open(root); err == nil {
		_ = index.Close()
		t.Fatal("Open followed a symlinked ancestor while creating the cache root")
	}
	if _, err := os.Lstat(filepath.Join(external, "new-cache")); !os.IsNotExist(err) {
		t.Fatalf("cache root was created through symlinked ancestor: %v", err)
	}
}

func TestIndexPersistenceSurvivesRootPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows root path locking is compile-checked on this host")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "cache")
	index := openIndex(t, root)
	defer index.Close()

	movedRoot := filepath.Join(parent, "cache-moved")
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementIndex := []byte("replacement root sentinel")
	if err := os.WriteFile(filepath.Join(root, "index.json"), replacementIndex, 0o600); err != nil {
		t.Fatal(err)
	}

	key := mustCacheKey(t, testHash, "320")
	entry := completeEntry(key, testCatalogHash, "320", "mp3", 8, time.Now())
	if err := index.Upsert(key, entry); err != nil {
		t.Fatalf("persist through anchored root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil || string(data) != string(replacementIndex) {
		t.Fatalf("replacement root was touched: data=%q err=%v", data, err)
	}
	movedData, err := os.ReadFile(filepath.Join(movedRoot, "index.json"))
	if err != nil || !strings.Contains(string(movedData), key) {
		t.Fatalf("anchored index was not updated: data=%q err=%v", movedData, err)
	}
}

func TestOpenRecoversCorruptIndex(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "flac")
	writeCacheFile(t, root, "audio/"+key+".flac", 17)
	writeCacheFile(t, root, "audio/not-a-cache.mp3", 5)
	writeCacheFile(t, root, "audio/"+key+".flac.extra", 5)
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	index := openIndex(t, root)
	defer index.Close()
	entry, ok, _ := index.Lookup(key)
	if !ok || !entry.Complete || entry.Size != 17 || entry.RelativePath != "audio/"+key+".flac" {
		t.Fatalf("rebuilt entry: %#v, %v", entry, ok)
	}
	if len(index.Snapshot()) != 1 {
		t.Fatalf("unexpected rebuilt entries: %#v", index.Snapshot())
	}
	backups, err := filepath.Glob(filepath.Join(root, "index.corrupt-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("corrupt backup: %v, %v", backups, err)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || string(data) != "{broken" {
		t.Fatalf("backup contents: %q, %v", data, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil || !json.Valid(data) {
		t.Fatalf("rebuilt index is unusable: %q, %v", data, err)
	}
}

func TestIndexRootRelativeFileOperations(t *testing.T) {
	root := t.TempDir()
	index := openIndex(t, root)
	defer index.Close()
	key := mustCacheKey(t, testHash, "128")
	part := "temp/" + key + ".part"
	final := "audio/" + key + ".mp3"

	file, err := index.OpenFile(part, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := index.RenameFile(part, final); err != nil {
		t.Fatal(err)
	}
	file, err = index.OpenFile(final, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "payload" {
		t.Fatalf("read final: %q, %v", data, err)
	}
	if _, err := index.OpenFile("../escape", os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		t.Fatal("unsafe relative open succeeded")
	}
}

func TestIndexConcurrentSnapshotsAndTouches(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, testHash, "super")
	writeCacheFile(t, root, "audio/"+key+".mp3", 4)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, testCatalogHash, "super", "mp3", 4, time.Now())); err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 20; iteration++ {
				_ = index.Touch(key, time.Unix(int64(worker*100+iteration), 0))
				_, _, _ = index.Lookup(key)
				_ = index.Snapshot()
			}
		}(worker)
	}
	workers.Wait()
}

func openIndex(t *testing.T, root string) *Index {
	t.Helper()
	index, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestOpenExistingNeverCreatesOrRepairsCandidate(t *testing.T) {
	t.Run("missing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		if _, err := OpenExisting(root); err == nil {
			t.Fatal("OpenExisting created a missing cache root")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("missing root was created: %v", err)
		}
	})

	t.Run("uninitialized root", func(t *testing.T) {
		root := t.TempDir()
		if _, err := OpenExisting(root); err == nil {
			t.Fatal("OpenExisting initialized an empty directory")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("OpenExisting created candidate contents: %v", entries)
		}
	})
}

func mustCacheKey(t *testing.T, hash, quality string) string {
	t.Helper()
	key, err := model.CacheKey(hash, quality, "none")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func completeEntry(key, catalogHash, quality, extension string, size int64, accessed time.Time) Entry {
	return Entry{
		Track: model.TrackInfo{
			Key: key, CatalogHash: catalogHash, Hash: strings.Split(key, ".")[0],
			RequestedQuality: quality, Quality: quality, Effect: "none", Extension: extension,
		},
		AliasKey:       strings.ToLower(catalogHash) + ":" + quality + ":none",
		RelativePath:   "audio/" + key + "." + extension,
		Size:           size,
		TotalBytes:     size,
		Ranges:         []model.ByteRange{{Start: 0, End: size}},
		Complete:       true,
		CreatedAt:      accessed.Add(-time.Hour),
		CompletedAt:    accessed.Add(-30 * time.Minute),
		LastAccessedAt: accessed,
	}
}

func writeCacheFile(t *testing.T, root, relative string, size int) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeIndexJSON(t *testing.T, root string, entries map[string]Entry) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
