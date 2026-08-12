package proxy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
)

func TestCacheControlProtectsActiveReferencesAndOmitsPaths(t *testing.T) {
	index, track, source := completedTask5Entry(t, t.TempDir(), []byte("protected cache"))
	defer index.Close()
	manager := NewManager(index, nil)
	defer manager.Close()
	release, err := manager.acquireReference(track.Key, nil, referenceExport)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := manager.DeleteCache(track.Key); deleted || !errors.Is(err, ErrCacheEntryActive) {
		t.Fatalf("active delete = %v, %v", deleted, err)
	}
	result, err := manager.ClearCache()
	if err != nil || len(result.Deleted) != 0 {
		t.Fatalf("active clear = %+v, %v", result, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("active cache file was removed: %v", err)
	}
	view, err := manager.CacheView()
	if err != nil || len(view.Items) != 1 || view.Items[0].Key != track.Key {
		t.Fatalf("cache view = %+v, %v", view, err)
	}
	release()
	if deleted, err := manager.DeleteCache(track.Key); err != nil || !deleted {
		t.Fatalf("released delete = %v, %v", deleted, err)
	}
}

func TestRevealTargetRequiresExactRecordOrRoot(t *testing.T) {
	index, _, cachePath := completedTask5Entry(t, t.TempDir(), []byte("cache"))
	defer index.Close()
	manager := NewManager(index, nil)
	defer manager.Close()
	downloadRoot := t.TempDir()
	recorded := filepath.Join(downloadRoot, "recorded.mp3")
	arbitrary := filepath.Join(downloadRoot, "arbitrary.mp3")
	for _, path := range []string{recorded, arbitrary} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.SetDownloadRoots(downloadRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDownloadRecords([]string{recorded}); err != nil {
		t.Fatal(err)
	}
	for kind, path := range map[string]string{
		"select-cache": cachePath, "select-download": recorded, "open-directory": downloadRoot,
	} {
		revealKind := "select"
		if kind == "open-directory" {
			revealKind = kind
		}
		if got, err := manager.RevealTarget(revealKind, path); err != nil || !samePath(got, path) {
			t.Fatalf("%s reveal = %q, %v", kind, got, err)
		}
	}
	if _, err := manager.RevealTarget("select", arbitrary); err == nil {
		t.Fatal("arbitrary file beneath a managed root was authorized")
	}
	if _, err := manager.RevealTarget("open-directory", filepath.Dir(recorded)+string(filepath.Separator)+"child"); err == nil {
		t.Fatal("arbitrary directory beneath a managed root was authorized")
	}
}

func TestManagerCloseClosesCurrentIndexAfterRootSwitch(t *testing.T) {
	startup, err := cache.Open(filepath.Join(t.TempDir(), "startup"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(startup, nil)
	if err := manager.SwitchCacheRoot(filepath.Join(t.TempDir(), "selected")); err != nil {
		t.Fatal(err)
	}
	selected := manager.index
	if selected == nil || selected == startup {
		t.Fatal("cache switch did not select a new index")
	}
	if _, err := startup.RootIdentity(); err == nil {
		t.Fatal("startup index remained open after cache switch")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := selected.RootIdentity(); err == nil {
		t.Fatal("manager close left the selected index open")
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
}

func TestManagerCloseClosesCurrentIndexAfterMigration(t *testing.T) {
	startup, _, _ := completedTask5Entry(t, t.TempDir(), []byte("migrated"))
	manager := NewManager(startup, nil)
	if err := manager.MigrateCache(filepath.Join(t.TempDir(), "selected"), false); err != nil {
		t.Fatal(err)
	}
	selected := manager.index
	if selected == nil || selected == startup {
		t.Fatal("cache migration did not select a new index")
	}
	if _, err := startup.RootIdentity(); err == nil {
		t.Fatal("startup index remained open after cache migration")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := selected.RootIdentity(); err == nil {
		t.Fatal("manager close left the migrated index open")
	}
}

func TestManagerCloseWaitsForMigrationAndClosesPublishedIndex(t *testing.T) {
	startup, track, _ := completedTask5Entry(t, t.TempDir(), []byte("concurrent migration"))
	manager := NewManager(startup, nil)
	release, err := manager.acquireReference(track.Key, nil, referenceExport)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "selected")
	migrated := make(chan error, 1)
	go func() { migrated <- manager.MigrateCache(destination, false) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		migrating := manager.migrating
		manager.mu.Unlock()
		if migrating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("migration did not acquire its lifecycle lease")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("close returned before migration completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close deadlocked after migration published")
	}
	manager.mu.Lock()
	selected := manager.index
	manager.mu.Unlock()
	if selected != nil {
		t.Fatal("manager retained a selected index after close")
	}
}

func TestCompleteCachePathUsesCurrentRootAndRootedValidation(t *testing.T) {
	startupRoot := t.TempDir()
	startup, track, source := completedTask5Entry(t, startupRoot, []byte("absolute cache path"))
	manager := NewManager(startup, nil)
	defer manager.Close()
	path, found, err := manager.CompleteCachePath(track.Key)
	if err != nil || !found || !filepath.IsAbs(path) || !samePath(path, source) {
		t.Fatalf("startup complete path = %q, %v, %v; want %q", path, found, err, source)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "absolute cache path" {
		t.Fatalf("startup complete contents = %q, %v", contents, err)
	}

	migratedRoot := filepath.Join(t.TempDir(), "migrated")
	if err := manager.MigrateCache(migratedRoot, false); err != nil {
		t.Fatal(err)
	}
	path, found, err = manager.CompleteCachePath(track.Key)
	want := filepath.Join(migratedRoot, "audio", track.Key+".mp3")
	if err != nil || !found || !samePath(path, want) {
		t.Fatalf("migrated complete path = %q, %v, %v; want %q", path, found, err, want)
	}
	if _, found, err := manager.CompleteCachePath("fedcba9876543210.320.none"); err != nil || found {
		t.Fatalf("missing complete path = %v, %v", found, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := manager.CompleteCachePath(track.Key); err != nil || found {
		t.Fatalf("stale complete path = %v, %v", found, err)
	}
}

func TestCompleteCachePathTouchesOnlyAfterSuccessfulValidation(t *testing.T) {
	index, track, source := completedTask5Entry(t, t.TempDir(), []byte("touch complete cache"))
	manager := NewManager(index, nil)
	defer manager.Close()

	original := index.Snapshot()[track.Key].LastAccessedAt
	touched := original.Add(2 * time.Hour).UTC()
	manager.controlNow = func() time.Time { return touched }
	path, found, err := manager.CompleteCachePath(track.Key)
	if err != nil || !found || !samePath(path, source) {
		t.Fatalf("complete path = %q, %v, %v", path, found, err)
	}
	if got := index.Snapshot()[track.Key].LastAccessedAt; !got.Equal(touched) {
		t.Fatalf("last accessed = %v, want %v", got, touched)
	}

	manager.touchCacheEntry = func(*cache.Index, string, time.Time) error {
		return errTouchCacheEntryForTest
	}
	path, found, err = manager.CompleteCachePath(track.Key)
	if path != "" || found || !errors.Is(err, errTouchCacheEntryForTest) {
		t.Fatalf("failed touch = %q, %v, %v", path, found, err)
	}
	if got := index.Snapshot()[track.Key].LastAccessedAt; !got.Equal(touched) {
		t.Fatalf("failed touch changed last accessed = %v, want %v", got, touched)
	}
	if _, found, err := manager.CompleteCachePath("fedcba9876543210.320.none"); err != nil || found {
		t.Fatalf("missing complete path = %v, %v", found, err)
	}
}

var errTouchCacheEntryForTest = errors.New("persist touch failed")

func TestCompleteCachePathDowngradesPostLookupStaleFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "missing", mutate: os.Remove},
		{name: "nonregular", mutate: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}},
		{name: "size mismatch", mutate: func(path string) error {
			return os.WriteFile(path, []byte("replacement with a different size"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			index, track, source := completedTask5Entry(t, t.TempDir(), []byte("stale complete cache"))
			manager := NewManager(index, nil)
			defer manager.Close()
			manager.afterCompleteLookup = func() {
				if err := test.mutate(source); err != nil {
					t.Fatal(err)
				}
			}

			path, found, err := manager.CompleteCachePath(track.Key)
			if err != nil || found || path != "" {
				t.Fatalf("stale complete path = %q, %v, %v", path, found, err)
			}
			if _, exists := index.Snapshot()[track.Key]; exists {
				t.Fatal("stale cache metadata remains indexed")
			}
		})
	}
}

func TestCompleteCachePathKeepsUnexpectedStatFailuresHard(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("hard stat failure"))
	manager := NewManager(index, nil)
	defer manager.Close()
	manager.statCacheEntry = func(*cache.Index, string) (os.FileInfo, error) {
		return nil, fs.ErrPermission
	}

	path, found, err := manager.CompleteCachePath(track.Key)
	if path != "" || found || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission failure = %q, %v, %v", path, found, err)
	}
	if _, exists := index.Snapshot()[track.Key]; !exists {
		t.Fatal("hard stat failure removed cache metadata")
	}
}
