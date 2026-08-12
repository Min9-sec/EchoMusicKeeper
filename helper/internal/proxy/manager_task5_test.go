package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

func TestExportWhenCompleteUsesRootedCacheFile(t *testing.T) {
	index, track, source := completedTask5Entry(t, t.TempDir(), []byte("permanent download"))
	defer index.Close()
	manager := NewManager(index, nil)
	root := t.TempDir()
	if err := manager.SetDownloadRoots(root, nil); err != nil {
		t.Fatal(err)
	}
	task, err := manager.ExportWhenComplete(track.Key, root)
	if err != nil {
		t.Fatal(err)
	}
	completed := waitTask5Task(t, manager, task.ID, model.TaskCompleted)
	if got, err := os.ReadFile(completed.OutputPath); err != nil || string(got) != "permanent download" {
		t.Fatalf("download bytes = %q, %v", got, err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "permanent download" {
		t.Fatalf("cache bytes = %q, %v", got, err)
	}
}

func TestSetLimitProtectsAcquiredExportUntilRelease(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("protected"))
	defer index.Close()
	manager := NewManager(index, nil)
	release, err := manager.acquireReference(track.Key, nil, referenceExport)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetLimit(0); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := index.Lookup(track.Key); !found {
		t.Fatal("active export was removed")
	}
	release()
	if _, found, _ := index.Lookup(track.Key); found {
		t.Fatal("cache entry remains after last export reference released")
	}
}

func TestMigrateCacheCopiesRootedFilesAndKeepsSourceByDefault(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("migrate me"))
	manager := NewManager(index, nil)
	newRoot := filepath.Join(t.TempDir(), "new-cache")
	if err := manager.MigrateCache(newRoot, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatalf("old root was removed: %v", err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("migrated lookup = %+v, %v", lookup, err)
	}
	if manager.index.Root() != filepath.Clean(newRoot) {
		t.Fatalf("manager root = %q, want %q", manager.index.Root(), filepath.Clean(newRoot))
	}
}

func TestMigrateCacheWaitsForReferencesAndDeletesOldRootWhenRequested(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("move me"))
	manager := NewManager(index, nil)
	session, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{
		Track: track, RemoteURL: "https://example.com/audio",
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := manager.acquireReference(track.Key, session, referenceExport)
	if err != nil {
		t.Fatal(err)
	}
	newRoot := filepath.Join(t.TempDir(), "migrated-cache")
	done := make(chan error, 1)
	go func() { done <- manager.MigrateCache(newRoot, true) }()
	select {
	case err := <-done:
		t.Fatalf("migration returned while a reference was active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if err := <-done; security.CanRemoveOpenedDirectory() {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "empty root retained") {
		t.Fatalf("migration error = %v, want retained empty old-root report", err)
	}
	if security.CanRemoveOpenedDirectory() {
		if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
			t.Fatalf("old root remains after deleteOld migration: %v", err)
		}
	} else if entries, err := os.ReadDir(oldRoot); err != nil || len(entries) != 0 {
		t.Fatalf("retained old root = %v, %v, want empty", entries, err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("migrated lookup = %+v, %v", lookup, err)
	}
}

func TestMigrateCacheRejectsNonemptyDestinationAndLeavesOldActive(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("stay active"))
	manager := NewManager(index, nil)
	newRoot := t.TempDir()
	marker := filepath.Join(newRoot, "user-file.txt")
	if err := os.WriteFile(marker, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.MigrateCache(newRoot, true); err == nil {
		t.Fatal("MigrateCache accepted a nonempty destination")
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("old cache lookup = %+v, %v", lookup, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "do not touch" {
		t.Fatalf("destination marker = %q, %v", got, err)
	}
}

func TestSwitchCacheRootStartsFreshWithoutDeletingOldRoot(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("leave me"))
	manager := NewManager(index, nil)
	newRoot := filepath.Join(t.TempDir(), "fresh-cache")
	if err := manager.SwitchCacheRoot(newRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatalf("old root was removed: %v", err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "miss" {
		t.Fatalf("fresh lookup = %+v, %v", lookup, err)
	}
}

func TestDeleteDownloadRequiresExactRecordAndAllowsOldManagedRoot(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	manager := NewManager(index, nil)
	current := t.TempDir()
	managed := t.TempDir()
	outside := t.TempDir()
	managedFile := filepath.Join(managed, "recorded.mp3")
	outsideFile := filepath.Join(outside, "outside.mp3")
	for _, path := range []string{managedFile, outsideFile} {
		if err := os.WriteFile(path, []byte("music"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.SetDownloadRoots(current, []string{managed, managed}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteDownload(managedFile); err == nil {
		t.Fatal("DeleteDownload accepted an unrecorded file")
	}
	if err := manager.SetDownloadRecords([]string{managedFile}); err != nil {
		t.Fatal(err)
	}
	newCurrent := t.TempDir()
	if err := manager.SetDownloadRoots(newCurrent, []string{managed}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteDownload(managedFile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managedFile); !os.IsNotExist(err) {
		t.Fatalf("managed download still exists: %v", err)
	}
	if err := manager.DeleteDownload(outsideFile); err == nil {
		t.Fatal("DeleteDownload accepted a path outside the allowlist")
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside download changed: %v", err)
	}
}

func TestDownloadTaskPauseResumeAndRetryTransitions(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	manager := NewManager(index, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	download := &downloadTask{
		manager: manager,
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		task: model.Task{
			ID: "download-actions", Kind: model.TaskKindDownload, State: model.TaskQueued,
			Pausable: true, Cancelable: true,
		},
	}
	manager.downloads[download.task.ID] = download
	if err := manager.Pause(download.task.ID); err != nil {
		t.Fatal(err)
	}
	if state := download.snapshot().State; state != model.TaskPaused {
		t.Fatalf("paused task state = %q", state)
	}
	if err := manager.Resume(download.task.ID); err != nil {
		t.Fatal(err)
	}
	if state := download.snapshot().State; state != model.TaskQueued {
		t.Fatalf("resumed task state = %q", state)
	}
	download.mu.Lock()
	download.task.State = model.TaskFailed
	download.task.Retryable = true
	download.mu.Unlock()
	if err := manager.Resume(download.task.ID); err != nil {
		t.Fatal(err)
	}
	if snapshot := download.snapshot(); snapshot.State != model.TaskQueued || snapshot.Retryable {
		t.Fatalf("retried task = %+v", snapshot)
	}
}

func TestCreateOrReuseStopsBeforeReplacingCompletedEntryOnValidationError(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("keep completed metadata"))
	manager := NewManager(index, nil)
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{
		Track: track, RemoteURL: "https://media.example.test/fresh?token=ephemeral",
	})
	if err == nil || !strings.Contains(err.Error(), "validate cache entry") {
		t.Fatalf("CreateOrReuse validation error = %v", err)
	}
	entry, found := index.Snapshot()[track.Key]
	if !found || !entry.Complete || entry.RelativePath == partialPath(track.Key) {
		t.Fatalf("completed metadata was replaced: found %t, entry %+v", found, entry)
	}
	manager.mu.Lock()
	session := manager.sessions[track.Key]
	manager.mu.Unlock()
	if session != nil {
		t.Fatalf("validation failure created session %+v", session.task())
	}
}

func completedTask5Entry(t *testing.T, root string, contents []byte) (*cache.Index, model.TrackInfo, string) {
	t.Helper()
	index, err := cache.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{
		Hash: "a1b2c3d4e5f60708", Quality: "320", Effect: "none",
		Artist: "Artist", Title: "Title", Extension: "mp3",
	}
	track.Key, err = model.CacheKey(track.Hash, track.Quality, track.Effect)
	if err != nil {
		t.Fatal(err)
	}
	relative := "audio/" + track.Key + ".mp3"
	file, err := index.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := index.Upsert(track.Key, cache.Entry{
		Track: track, RelativePath: relative, Size: int64(len(contents)), TotalBytes: int64(len(contents)),
		Ranges: []model.ByteRange{{Start: 0, End: int64(len(contents))}}, Complete: true,
		CreatedAt: now, CompletedAt: now, LastAccessedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return index, track, filepath.Join(root, filepath.FromSlash(relative))
}

func waitTask5Task(t *testing.T, manager *Manager, id string, state model.TaskState) model.Task {
	t.Helper()
	deadline, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		for _, task := range manager.Tasks() {
			if task.ID == id && task.State == state {
				return task
			}
		}
		select {
		case <-deadline.Done():
			t.Fatalf("task %q did not reach %q", id, state)
		case <-time.After(time.Millisecond):
		}
	}
}
