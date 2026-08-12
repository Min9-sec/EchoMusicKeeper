package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

func TestFailedAndPausedDownloadsReleaseReferenceForMigration(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		oldRoot := t.TempDir()
		index, track, _ := completedTask5Entry(t, oldRoot, []byte("failed export"))
		manager := NewManager(index, nil)
		manager.copyDownload = func(context.Context, io.Reader, int64, *security.Directory, model.TrackInfo) (string, error) {
			return "", errors.New("injected export failure")
		}
		downloadRoot := t.TempDir()
		task, err := manager.ExportWhenComplete(track.Key, downloadRoot)
		if err != nil {
			t.Fatal(err)
		}
		waitTask5Task(t, manager, task.ID, model.TaskFailed)
		assertNoTask5Reference(t, manager, track.Key)
		if err := migrateTask5Within(manager, filepath.Join(t.TempDir(), "failed-migration")); err != nil {
			t.Fatal(err)
		}
		if err := manager.Cancel(task.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("paused", func(t *testing.T) {
		oldRoot := t.TempDir()
		index, track, _ := completedTask5Entry(t, oldRoot, []byte("paused export"))
		manager := NewManager(index, nil)
		started := make(chan struct{})
		manager.copyDownload = func(ctx context.Context, _ io.Reader, _ int64, _ *security.Directory, _ model.TrackInfo) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		task, err := manager.ExportWhenComplete(track.Key, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		<-started
		if err := manager.Pause(task.ID); err != nil {
			t.Fatal(err)
		}
		waitTask5Task(t, manager, task.ID, model.TaskPaused)
		waitNoTask5Reference(t, manager, track.Key)
		if err := migrateTask5Within(manager, filepath.Join(t.TempDir(), "paused-migration")); err != nil {
			t.Fatal(err)
		}
		if err := manager.Cancel(task.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPauseResumeIgnoresCanceledOlderAttempt(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("resume export"))
	manager := NewManager(index, nil)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	attempts := 0
	manager.copyDownload = func(ctx context.Context, _ io.Reader, _ int64, root *security.Directory, _ model.TrackInfo) (string, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			close(firstStarted)
			<-ctx.Done()
			<-releaseFirst
			return "", ctx.Err()
		}
		output, err := root.OpenFile("resumed.mp3", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		if _, err := output.Write([]byte("complete")); err != nil {
			_ = output.Close()
			return "", err
		}
		return "resumed.mp3", output.Close()
	}
	task, err := manager.ExportWhenComplete(track.Key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	if err := manager.Pause(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resume(task.ID); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	completed := waitTask5Task(t, manager, task.ID, model.TaskCompleted)
	if completed.Error != "" {
		t.Fatalf("resumed task retained an older attempt error: %q", completed.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("copy attempts = %d, want 2", attempts)
	}
}

func TestStaleAttemptCleanupUsesOriginalDownloadDirectory(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("stale output"))
			manager := NewManager(index, nil)
			parent := t.TempDir()
			root := filepath.Join(parent, "music")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			firstWritten := make(chan struct{})
			releaseFirst := make(chan struct{})
			secondStarted := make(chan struct{})
			var calls int
			manager.copyDownload = func(ctx context.Context, _ io.Reader, _ int64, outputRoot *security.Directory, _ model.TrackInfo) (string, error) {
				calls++
				if calls == 1 {
					output, err := outputRoot.OpenFile("same.mp3", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
					if err != nil {
						return "", err
					}
					if _, err := output.Write([]byte("stale")); err != nil {
						_ = output.Close()
						return "", err
					}
					if err := output.Close(); err != nil {
						return "", err
					}
					close(firstWritten)
					<-releaseFirst
					return "same.mp3", nil
				}
				close(secondStarted)
				<-ctx.Done()
				return "", ctx.Err()
			}
			task, err := manager.ExportWhenComplete(track.Key, root)
			if err != nil {
				t.Fatal(err)
			}
			<-firstWritten
			if err := manager.Pause(task.ID); err != nil {
				t.Fatal(err)
			}
			if err := manager.Resume(task.ID); err != nil {
				t.Fatal(err)
			}
			displaced := root + ".displaced"
			if err := os.Rename(root, displaced); err != nil {
				t.Fatal(err)
			}
			replacementRoot := root
			if replacement == "symlink" {
				replacementRoot = filepath.Join(parent, "outside")
				if err := os.MkdirAll(replacementRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("outside", root); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			replacementFile := filepath.Join(replacementRoot, "same.mp3")
			if err := os.WriteFile(replacementFile, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			close(releaseFirst)
			<-secondStarted
			if _, err := os.Stat(filepath.Join(displaced, "same.mp3")); !os.IsNotExist(err) {
				t.Fatalf("stale output remains in original directory: %v", err)
			}
			if got, err := os.ReadFile(replacementFile); err != nil || string(got) != "replacement" {
				t.Fatalf("replacement output = %q, %v", got, err)
			}
			if err := manager.Cancel(task.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStaleAttemptCleanupFailureIsRecorded(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("cleanup error"))
	manager := NewManager(index, nil)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls int
	manager.copyDownload = func(ctx context.Context, _ io.Reader, _ int64, _ *security.Directory, _ model.TrackInfo) (string, error) {
		calls++
		if calls == 1 {
			close(firstStarted)
			<-releaseFirst
			return "stale.mp3", nil
		}
		close(secondStarted)
		<-ctx.Done()
		return "", ctx.Err()
	}
	manager.cleanupDownload = func(*security.Directory, string) error {
		return errors.New("injected cleanup denial")
	}
	task, err := manager.ExportWhenComplete(track.Key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	if err := manager.Pause(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resume(task.ID); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	<-secondStarted
	var snapshot model.Task
	for _, current := range manager.Tasks() {
		if current.ID == task.ID {
			snapshot = current
			break
		}
	}
	if !strings.Contains(snapshot.Error, "injected cleanup denial") {
		t.Fatalf("resumed task error = %q, want stale cleanup failure", snapshot.Error)
	}
	if err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulExportRegistersExactDeleteRecord(t *testing.T) {
	index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("registered"))
	manager := NewManager(index, nil)
	downloadRoot := t.TempDir()
	if err := manager.SetDownloadRoots(downloadRoot, nil); err != nil {
		t.Fatal(err)
	}
	task, err := manager.ExportWhenComplete(track.Key, downloadRoot)
	if err != nil {
		t.Fatal(err)
	}
	completed := waitTask5Task(t, manager, task.ID, model.TaskCompleted)
	if err := manager.DeleteDownload(completed.OutputPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(completed.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("registered export still exists: %v", err)
	}
}

func TestDownloadCompletionPublishesRecordAndStateAtomically(t *testing.T) {
	t.Run("tasks then delete", func(t *testing.T) {
		index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("atomic publish"))
		manager := NewManager(index, nil)
		root := t.TempDir()
		if err := manager.SetDownloadRoots(root, nil); err != nil {
			t.Fatal(err)
		}
		reached := make(chan struct{})
		release := make(chan struct{})
		manager.beforeDownloadPublish = func() {
			close(reached)
			<-release
		}
		task, err := manager.ExportWhenComplete(track.Key, root)
		if err != nil {
			t.Fatal(err)
		}
		<-reached
		for _, snapshot := range manager.Tasks() {
			if snapshot.ID == task.ID && snapshot.State == model.TaskCompleted {
				t.Fatal("TaskCompleted became visible before its delete record")
			}
		}
		close(release)
		completed := waitTask5Task(t, manager, task.ID, model.TaskCompleted)
		if err := manager.DeleteDownload(completed.OutputPath); err != nil {
			t.Fatalf("immediate completed-task deletion failed: %v", err)
		}
	})

	t.Run("close wins publication", func(t *testing.T) {
		index, track, _ := completedTask5Entry(t, t.TempDir(), []byte("close publish"))
		manager := NewManager(index, nil)
		root := t.TempDir()
		reached := make(chan struct{})
		release := make(chan struct{})
		manager.beforeDownloadPublish = func() {
			close(reached)
			<-release
		}
		task, err := manager.ExportWhenComplete(track.Key, root)
		if err != nil {
			t.Fatal(err)
		}
		<-reached
		closed := make(chan error, 1)
		go func() { closed <- manager.Close() }()
		deadline := time.Now().Add(time.Second)
		for {
			manager.mu.Lock()
			isClosed := manager.closed
			manager.mu.Unlock()
			if isClosed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Close did not enter the closed state")
			}
			time.Sleep(time.Millisecond)
		}
		close(release)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		for _, snapshot := range manager.Tasks() {
			if snapshot.ID == task.ID && snapshot.State == model.TaskCompleted {
				t.Fatal("download completed after Close won publication")
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("close-winning publication left output: %v", entries)
		}
	})
}

func TestDeleteDownloadRejectsRootReplacementWithoutTouchingTarget(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(index, nil)
	parent := t.TempDir()
	root := filepath.Join(parent, "music")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(root, "recorded.mp3")
	if err := os.WriteFile(recorded, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "recorded.mp3")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDownloadRoots(root, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDownloadRecords([]string{recorded}); err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	proceed := make(chan struct{})
	manager.beforeDownloadDelete = func() {
		close(opened)
		<-proceed
	}
	done := make(chan error, 1)
	go func() { done <- manager.DeleteDownload(recorded) }()
	<-opened
	displaced := root + ".displaced"
	if err := os.Rename(root, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-done; err == nil {
		t.Fatal("DeleteDownload accepted a replaced managed root")
	}
	for path, want := range map[string]string{
		filepath.Join(displaced, "recorded.mp3"): "inside",
		outsideFile:                              "outside",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("file %q = %q, %v, want %q", path, got, err, want)
		}
	}
}

func TestRenameMigrationFailureNeverSelectsClosedIndex(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("recovery"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	defaultOps := manager.migrationOps
	openCalls := 0
	manager.migrationOps.sameVolume = func(string, string) bool { return true }
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		openCalls++
		if samePath(path, destination) || samePath(path, oldRoot) {
			return nil, fmt.Errorf("injected open failure for %s", filepath.Base(path))
		}
		return defaultOps.openExisting(path)
	}
	renameCalls := 0
	manager.migrationOps.rename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 1 {
			return os.Rename(oldPath, newPath)
		}
		return errors.New("injected rollback failure")
	}
	if err := manager.MigrateCache(destination, true); err == nil {
		t.Fatal("MigrateCache succeeded despite open and rollback failures")
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err == nil || !strings.Contains(err.Error(), "cache manager unavailable") {
		t.Fatalf("Lookup() = %+v, %v, want deterministic unavailable error", lookup, err)
	}
	for name, operation := range map[string]func() error{
		"create": func() error {
			_, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{
				Track: track, RemoteURL: "https://example.com/audio",
			})
			return err
		},
		"update URL": func() error { return manager.UpdateURL(track.Key, "https://example.com/retry") },
		"set limit":  func() error { return manager.SetLimit(defaultCacheLimit) },
	} {
		if err := operation(); err == nil || !strings.Contains(err.Error(), "cache manager unavailable") {
			t.Fatalf("%s error = %v, want deterministic unavailable error", name, err)
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case <-time.After(time.Second):
		t.Fatal("Close blocked after unrecoverable migration")
	case <-closed:
	}
}

func TestRenameMigrationRecoveryCanSelectMovedDestination(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("destination recovery"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	defaultOps := manager.migrationOps
	destinationOpenCalls := 0
	manager.migrationOps.sameVolume = func(string, string) bool { return true }
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if samePath(path, destination) {
			destinationOpenCalls++
			if destinationOpenCalls == 1 {
				return nil, errors.New("injected first destination open failure")
			}
		}
		return defaultOps.openExisting(path)
	}
	renameCalls := 0
	manager.migrationOps.rename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 1 {
			return os.Rename(oldPath, newPath)
		}
		return errors.New("injected rollback failure")
	}
	if err := manager.MigrateCache(destination, true); err == nil {
		t.Fatal("MigrateCache succeeded despite the recoverable publish failure")
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("recovered lookup = %+v, %v", lookup, err)
	}
	if manager.index == nil || !samePath(manager.index.Root(), destination) {
		t.Fatalf("recovered root = %v, want %q", manager.index, destination)
	}
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("recovery created a missing old root: %v", err)
	}
}

func TestRenameMigrationRejectsReplacementRecoveryCandidate(t *testing.T) {
	for _, contents := range [][]byte{nil, []byte("AAAA")} {
		name := "empty"
		if contents != nil {
			name = "matching metadata"
		}
		t.Run(name, func(t *testing.T) {
			oldRoot := t.TempDir()
			var index *cache.Index
			var track model.TrackInfo
			if contents == nil {
				var err error
				index, err = cache.Open(oldRoot)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				index, track, _ = completedTask5Entry(t, oldRoot, contents)
			}
			entries := index.Snapshot()
			manager := NewManager(index, nil)
			destination := filepath.Join(t.TempDir(), "destination")
			genuine := destination + ".genuine"
			defaultOps := manager.migrationOps
			manager.migrationOps.sameVolume = func(string, string) bool { return true }
			manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
				if samePath(path, destination) {
					if err := os.Rename(destination, genuine); err != nil {
						return nil, err
					}
					replacement, err := cache.Open(destination)
					if err != nil {
						return nil, err
					}
					for key, entry := range entries {
						file, err := replacement.OpenFile(entry.RelativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
						if err != nil {
							return nil, err
						}
						if _, err := file.Write([]byte("BBBB")); err != nil {
							return nil, err
						}
						if err := file.Close(); err != nil {
							return nil, err
						}
						if err := replacement.Upsert(key, entry); err != nil {
							return nil, err
						}
					}
					return replacement, nil
				}
				return defaultOps.openExisting(path)
			}
			if err := manager.MigrateCache(destination, true); err == nil {
				t.Fatal("MigrateCache selected a replacement recovery candidate")
			}
			if manager.index != nil && samePath(manager.index.Root(), destination) {
				t.Fatal("replacement directory became the active cache index")
			}
			if contents != nil {
				if lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key}); err == nil && lookup.Status == "complete" {
					t.Fatal("replacement cache content was exposed as migrated data")
				}
			}
		})
	}
}

func TestMigrationValidationChecksFullMetadataAndContent(t *testing.T) {
	for _, mutation := range []string{"content", "metadata"} {
		t.Run(mutation, func(t *testing.T) {
			oldRoot := t.TempDir()
			index, track, _ := completedTask5Entry(t, oldRoot, []byte("AAAA"))
			manager := NewManager(index, nil)
			defaultOpen := manager.migrationOps.openExisting
			mutated := false
			manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
				if !mutated && strings.Contains(filepath.Base(path), ".migration-") {
					mutated = true
					entry := index.Snapshot()[track.Key]
					switch mutation {
					case "content":
						if err := os.WriteFile(filepath.Join(path, filepath.FromSlash(entry.RelativePath)), []byte("BBBB"), 0o600); err != nil {
							return nil, err
						}
					case "metadata":
						data, err := os.ReadFile(filepath.Join(path, "index.json"))
						if err != nil {
							return nil, err
						}
						var entries map[string]cache.Entry
						if err := json.Unmarshal(data, &entries); err != nil {
							return nil, err
						}
						changed := entries[track.Key]
						changed.LastAccessedAt = changed.LastAccessedAt.Add(time.Second)
						entries[track.Key] = changed
						data, err = json.Marshal(entries)
						if err != nil {
							return nil, err
						}
						if err := os.WriteFile(filepath.Join(path, "index.json"), data, 0o600); err != nil {
							return nil, err
						}
					}
				}
				return defaultOpen(path)
			}
			if err := manager.MigrateCache(filepath.Join(t.TempDir(), "destination"), false); err == nil {
				t.Fatalf("MigrateCache accepted changed %s", mutation)
			}
			lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
			if err != nil || lookup.Status != "complete" {
				t.Fatalf("original lookup = %+v, %v", lookup, err)
			}
		})
	}
}

func TestCopyMigrationRejectsReplacedOldRootDuringExplicitDeletion(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("preserve original"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	displaced := oldRoot + ".displaced"
	replacementMarker := filepath.Join(oldRoot, "replacement.txt")
	manager.beforeOldRootDelete = func() {
		if err := os.Rename(oldRoot, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(oldRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacementMarker, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "destination")
	err := manager.MigrateCache(destination, true)
	if security.CanRemoveOpenedDirectory() {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "empty root retained") {
		t.Fatalf("MigrateCache error = %v, want retained empty old-root report", err)
	}
	assertTask5RemovedOrRetainedEmpty(t, displaced)
	if got, err := os.ReadFile(replacementMarker); err != nil || string(got) != "outside" {
		t.Fatalf("replacement marker = %q, %v", got, err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("selected destination lookup = %+v, %v", lookup, err)
	}
}

func TestCopyMigrationStagingCleanupCannotFollowReplacement(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("staging cleanup"))
	manager := NewManager(index, nil)
	defaultOps := manager.migrationOps
	stagingOpenCalls := 0
	manager.migrationOps.openIndex = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			stagingOpenCalls++
		}
		return defaultOps.openIndex(path)
	}
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			return nil, errors.New("injected staging reopen failure")
		}
		return defaultOps.openExisting(path)
	}
	var displaced, replacementMarker string
	manager.beforeStagingCleanup = func(staging string) {
		displaced = staging + ".displaced"
		if err := os.Rename(staging, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		replacementMarker = filepath.Join(staging, "replacement.txt")
		if err := os.WriteFile(replacementMarker, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := manager.MigrateCache(filepath.Join(t.TempDir(), "destination"), false)
	if err == nil {
		t.Fatal("MigrateCache succeeded despite the staging reopen failure")
	}
	if !security.CanRemoveOpenedDirectory() && !strings.Contains(err.Error(), "staging cleanup") {
		t.Fatalf("MigrateCache error = %v, want staging cleanup failure", err)
	}
	if got, err := os.ReadFile(replacementMarker); err != nil || string(got) != "outside" {
		t.Fatalf("replacement marker = %q, %v", got, err)
	}
	assertTask5RemovedOrRetainedEmpty(t, displaced)
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after staging failure = %+v, %v", lookup, err)
	}
}

func TestFailedStagingDirectoryIsRemoved(t *testing.T) {
	oldRoot := t.TempDir()
	index, _, _ := completedTask5Entry(t, oldRoot, []byte("cleanup directory"))
	manager := NewManager(index, nil)
	defaultOps := manager.migrationOps
	var stagingRoot string
	manager.migrationOps.openIndex = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			stagingRoot = path
		}
		return defaultOps.openIndex(path)
	}
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			return nil, errors.New("injected staging reopen failure")
		}
		return defaultOps.openExisting(path)
	}
	destination := filepath.Join(t.TempDir(), "destination")
	if err := manager.MigrateCache(destination, false); err == nil {
		t.Fatal("MigrateCache succeeded despite staging reopen failure")
	}
	if security.CanRemoveOpenedDirectory() {
		if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
			t.Fatalf("failed staging directory remains: %v", err)
		}
	} else if entries, err := os.ReadDir(stagingRoot); err != nil || len(entries) != 0 {
		t.Fatalf("retained staging directory = %v, %v, want empty", entries, err)
	}
	if !security.CanRemoveOpenedDirectory() {
		err := manager.MigrateCache(destination, false)
		if err == nil || !strings.Contains(err.Error(), stagingRoot) || !strings.Contains(err.Error(), "remove the retained empty directory before retry") {
			t.Fatalf("retry error = %v, want actionable bounded-residual path", err)
		}
		matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".migration-*"))
		if globErr != nil || len(matches) != 1 || !samePath(matches[0], stagingRoot) {
			t.Fatalf("staging residuals = %v, %v, want only %q", matches, globErr, stagingRoot)
		}
	}
}

func TestCopyMigrationFailedPublishedRollbackCleansDestinationAndCanRetry(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("published cleanup"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	defaultOps := manager.migrationOps
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	destinationOpenFailed := false
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if samePath(path, destination) && !destinationOpenFailed {
			destinationOpenFailed = true
			return nil, errors.New("injected published destination reopen failure")
		}
		return defaultOps.openExisting(path)
	}
	manager.migrationOps.rename = func(oldPath, newPath string) error {
		if samePath(oldPath, destination) {
			return errors.New("injected published rollback failure")
		}
		return defaultOps.rename(oldPath, newPath)
	}

	err := manager.MigrateCache(destination, false)
	if err == nil || !strings.Contains(err.Error(), "injected published destination reopen failure") ||
		!strings.Contains(err.Error(), "injected published rollback failure") {
		t.Fatalf("MigrateCache error = %v, want reopen and rollback failures", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatalf("read cleaned published destination: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cleaned published destination is not empty: %v", entries)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after failed publish = %+v, %v", lookup, err)
	}
	if err := manager.MigrateCache(destination, false); err != nil {
		t.Fatalf("retry migration after published cleanup: %v", err)
	}
	if manager.index == nil || !samePath(manager.index.Root(), destination) {
		t.Fatalf("retried migration root = %v, want %q", manager.index, destination)
	}
}

func TestCopyMigrationFailedPublishedCleanupPreservesReplacement(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("published replacement"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	displaced := destination + ".displaced"
	replacementMarker := filepath.Join(destination, "replacement.txt")
	defaultOps := manager.migrationOps
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if samePath(path, destination) {
			return nil, errors.New("injected replacement destination reopen failure")
		}
		return defaultOps.openExisting(path)
	}
	manager.migrationOps.rename = func(oldPath, newPath string) error {
		if samePath(oldPath, destination) {
			if err := os.Rename(destination, displaced); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(destination, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacementMarker, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			return errors.New("injected replacement rollback failure")
		}
		return defaultOps.rename(oldPath, newPath)
	}

	err := manager.MigrateCache(destination, false)
	if err == nil || !strings.Contains(err.Error(), "injected replacement destination reopen failure") ||
		!strings.Contains(err.Error(), "injected replacement rollback failure") {
		t.Fatalf("MigrateCache error = %v, want reopen and rollback failures", err)
	}
	if strings.Contains(err.Error(), "published destination cleanup") {
		t.Fatalf("MigrateCache reported pathname cleanup after capability cleanup: %v", err)
	}
	if got, err := os.ReadFile(replacementMarker); err != nil || string(got) != "outside" {
		t.Fatalf("replacement marker = %q, %v", got, err)
	}
	entries, err := os.ReadDir(displaced)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("owned published tree was not cleaned through its capability: %v", entries)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after failed cleanup = %+v, %v", lookup, err)
	}
}

func TestCopyMigrationNeverUnlinksPublishedDestinationAfterCleanup(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("post-cleanup replacement"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	displaced := destination + ".displaced"
	replacementMarker := filepath.Join(destination, "replacement.txt")
	defaultOps := manager.migrationOps
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if samePath(path, destination) {
			return nil, errors.New("injected post-cleanup destination reopen failure")
		}
		return defaultOps.openExisting(path)
	}
	manager.migrationOps.rename = func(oldPath, newPath string) error {
		if samePath(oldPath, destination) {
			return errors.New("injected post-cleanup rollback failure")
		}
		return defaultOps.rename(oldPath, newPath)
	}
	manager.afterPublishedCleanup = func() {
		if err := os.Rename(destination, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacementMarker, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := manager.MigrateCache(destination, false)
	if err == nil || !strings.Contains(err.Error(), "injected post-cleanup destination reopen failure") ||
		!strings.Contains(err.Error(), "injected post-cleanup rollback failure") {
		t.Fatalf("MigrateCache error = %v, want reopen and rollback failures", err)
	}
	if strings.Contains(err.Error(), "published destination cleanup") {
		t.Fatalf("MigrateCache reported pathname cleanup after capability cleanup: %v", err)
	}
	if got, err := os.ReadFile(replacementMarker); err != nil || string(got) != "outside" {
		t.Fatalf("replacement marker = %q, %v", got, err)
	}
	entries, err := os.ReadDir(displaced)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("displaced owned destination is not empty: %v", entries)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after post-cleanup replacement = %+v, %v", lookup, err)
	}
}

func TestExistingDestinationFinalCheckReplacementIsUntouched(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("existing destination race"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	destination := t.TempDir()
	var marker string
	manager.afterDestinationCheck = func() {
		_, marker = replaceTask5DirectoryWithSymlink(t, destination)
	}

	if err := manager.MigrateCache(destination, false); err == nil {
		t.Fatal("MigrateCache succeeded after the existing destination was replaced")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside" {
		t.Fatalf("existing destination replacement marker = %q, %v", got, err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("existing destination replacement = %v, %v, want symlink", info, err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after destination replacement = %+v, %v", lookup, err)
	}
}

func TestExistingEmptyDestinationMigrationUsesSafePlatformBehavior(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("existing destination"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	destination := t.TempDir()
	before, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.MigrateCache(destination, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !security.CanRemoveOpenedDirectory() && !os.SameFile(before, after) {
		t.Fatal("non-Windows migration replaced the retained empty destination identity")
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("existing destination lookup = %+v, %v", lookup, err)
	}
}

func TestRetainedDestinationReplacementBeforeInitializationIsUntouched(t *testing.T) {
	if security.CanRemoveOpenedDirectory() {
		t.Skip("Windows removes the existing empty destination by handle")
	}
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("retained destination race"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	destination := t.TempDir()
	var displaced, marker string
	manager.afterDestinationClone = func() {
		displaced, marker = replaceTask5DirectoryWithSymlink(t, destination)
	}

	if err := manager.MigrateCache(destination, false); err == nil {
		t.Fatal("MigrateCache initialized a replaced retained destination")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside" {
		t.Fatalf("retained destination replacement marker = %q, %v", got, err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("retained destination replacement = %v, %v, want symlink", info, err)
	}
	if entries, err := os.ReadDir(displaced); err != nil || len(entries) != 0 {
		t.Fatalf("retained destination identity = %v, %v, want empty", entries, err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after retained destination replacement = %+v, %v", lookup, err)
	}
}

func TestOldRootFinalCheckReplacementIsUntouched(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("old root race"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	var displaced, marker string
	manager.afterOldRootCleanup = func() {
		displaced, marker = replaceTask5DirectoryWithSymlink(t, oldRoot)
	}
	destination := filepath.Join(t.TempDir(), "destination")

	err := manager.MigrateCache(destination, true)
	if security.CanRemoveOpenedDirectory() {
		if err != nil {
			t.Fatal(err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "empty root retained") {
		t.Fatalf("MigrateCache error = %v, want retained empty old-root report", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside" {
		t.Fatalf("old-root replacement marker = %q, %v", got, err)
	}
	if info, err := os.Lstat(oldRoot); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("old-root replacement = %v, %v, want symlink", info, err)
	}
	assertTask5RemovedOrRetainedEmpty(t, displaced)
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("migrated lookup after old-root replacement = %+v, %v", lookup, err)
	}
}

func TestStagingFinalCheckReplacementIsUntouched(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("staging race"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return false }
	defaultOpen := manager.migrationOps.openExisting
	var stagingRoot, displaced, marker string
	manager.migrationOps.openIndex = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			stagingRoot = path
		}
		return cache.Open(path)
	}
	manager.migrationOps.openExisting = func(path string) (*cache.Index, error) {
		if samePath(path, stagingRoot) {
			return nil, errors.New("injected staging validation failure")
		}
		return defaultOpen(path)
	}
	manager.afterStagingCleanup = func() {
		displaced, marker = replaceTask5DirectoryWithSymlink(t, stagingRoot)
	}

	if err := manager.MigrateCache(filepath.Join(t.TempDir(), "destination"), false); err == nil {
		t.Fatal("MigrateCache succeeded despite staging validation failure")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside" {
		t.Fatalf("staging replacement marker = %q, %v", got, err)
	}
	if info, err := os.Lstat(stagingRoot); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("staging replacement = %v, %v, want symlink", info, err)
	}
	assertTask5RemovedOrRetainedEmpty(t, displaced)
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("original lookup after staging replacement = %+v, %v", lookup, err)
	}
}

func TestRenameProbeFinalCheckReplacementIsUntouched(t *testing.T) {
	oldRoot := t.TempDir()
	index, track, _ := completedTask5Entry(t, oldRoot, []byte("probe race"))
	manager := NewManager(index, nil)
	manager.migrationOps.sameVolume = func(string, string) bool { return true }
	defaultOpen := manager.migrationOps.openIndex
	var probeRoot, marker string
	manager.migrationOps.openIndex = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			probeRoot = path
		}
		return defaultOpen(path)
	}
	manager.afterStagingCleanup = func() {
		_, marker = replaceTask5DirectoryWithSymlink(t, probeRoot)
	}
	destination := filepath.Join(t.TempDir(), "destination")

	if err := manager.MigrateCache(destination, true); err != nil {
		t.Fatalf("rename migration with retained probe: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "outside" {
		t.Fatalf("probe replacement marker = %q, %v", got, err)
	}
	if info, err := os.Lstat(probeRoot); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("probe replacement = %v, %v, want symlink", info, err)
	}
	lookup, err := manager.Lookup(model.CacheLookupRequest{Key: track.Key})
	if err != nil || lookup.Status != "complete" {
		t.Fatalf("renamed lookup after probe replacement = %+v, %v", lookup, err)
	}
}

func TestStagingCreateFailureReturnsCleanupErrorAndRemovesDirectory(t *testing.T) {
	index, _, _ := completedTask5Entry(t, t.TempDir(), []byte("create cleanup"))
	manager := NewManager(index, nil)
	defaultCleanup := manager.migrationOps.cleanupStaging
	var stagingRoot string
	manager.migrationOps.openIndex = func(path string) (*cache.Index, error) {
		if strings.Contains(filepath.Base(path), ".migration-") {
			stagingRoot = path
			return nil, errors.New("injected staging index open failure")
		}
		return cache.Open(path)
	}
	manager.migrationOps.cleanupStaging = func(directory *security.Directory, afterCheck func()) (bool, error) {
		removed, cleanupErr := defaultCleanup(directory, afterCheck)
		return removed, errors.Join(cleanupErr, errors.New("injected cleanup report"))
	}
	err := manager.MigrateCache(filepath.Join(t.TempDir(), "destination"), false)
	if err == nil || !strings.Contains(err.Error(), "injected cleanup report") {
		t.Fatalf("MigrateCache error = %v, want cleanup failure", err)
	}
	if security.CanRemoveOpenedDirectory() {
		if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
			t.Fatalf("partially created staging directory remains: %v", err)
		}
	} else if entries, err := os.ReadDir(stagingRoot); err != nil || len(entries) != 0 {
		t.Fatalf("retained partial staging directory = %v, %v, want empty", entries, err)
	}
}

func TestStagingDirectoryCreationFailureIsPropagated(t *testing.T) {
	index, _, _ := completedTask5Entry(t, t.TempDir(), []byte("create failure"))
	manager := NewManager(index, nil)
	destination := filepath.Join(t.TempDir(), "destination")
	staging := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".migration-staging")
	openErr := errors.New("injected created-directory open failure")
	createErr := errors.Join(
		openErr,
		fmt.Errorf("directory creation at %q requires manual residual cleanup", staging),
	)
	calls := 0
	manager.migrationOps.createDirectory = func(path string) (*security.Directory, error) {
		calls++
		if path != staging {
			t.Fatalf("create directory path = %q, want %q", path, staging)
		}
		return nil, createErr
	}

	err := manager.MigrateCache(destination, false)
	if !errors.Is(err, openErr) {
		t.Fatalf("MigrateCache error = %v, want original directory-open failure", err)
	}
	if !strings.Contains(err.Error(), "create cache migration staging root") || !strings.Contains(err.Error(), staging) {
		t.Fatalf("MigrateCache error = %v, want staging context and residual path", err)
	}
	if calls != 1 {
		t.Fatalf("create directory calls = %d, want one bounded attempt", calls)
	}
}

func mustRelativeTask5(t *testing.T, root, path string) string {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}

func replaceTask5DirectoryWithSymlink(t *testing.T, path string) (string, string) {
	t.Helper()
	displaced := path + ".displaced"
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "replacement.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	return displaced, filepath.Join(path, "replacement.txt")
}

func assertTask5RemovedOrRetainedEmpty(t *testing.T, path string) {
	t.Helper()
	if security.CanRemoveOpenedDirectory() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("identity-bound directory remains at %q: %v", path, err)
		}
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("retained directory %q is not empty: %v", path, entries)
	}
}

func assertNoTask5Reference(t *testing.T, manager *Manager, key string) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.references[key] != 0 {
		t.Fatalf("reference count for %q = %d, want 0", key, manager.references[key])
	}
}

func waitNoTask5Reference(t *testing.T, manager *Manager, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		count := manager.references[key]
		manager.mu.Unlock()
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reference count for %q remained %d", key, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func migrateTask5Within(manager *Manager, root string) error {
	done := make(chan error, 1)
	go func() { done <- manager.MigrateCache(root, false) }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		return errors.New("migration remained blocked by a parked download")
	}
}
