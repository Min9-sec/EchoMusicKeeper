package cache

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func TestCleanToPrunesPartialsThenUsesLRU(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	oldKey := mustCacheKey(t, "0000000000000001", "320")
	newKey := mustCacheKey(t, "0000000000000002", "320")
	partialKey := mustCacheKey(t, "0000000000000003", "320")
	writeCacheFile(t, root, "audio/"+oldKey+".mp3", 80)
	writeCacheFile(t, root, "audio/"+newKey+".mp3", 80)
	writeCacheFile(t, root, "temp/"+partialKey+".part", 20)

	index := openIndex(t, root)
	if err := index.Upsert(oldKey, completeEntry(oldKey, "1000000000000001", "320", "mp3", 80, now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert(newKey, completeEntry(newKey, "1000000000000002", "320", "mp3", 80, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	partial := Entry{
		Track:          model.TrackInfo{Key: partialKey, Hash: "0000000000000003", Quality: "320", Effect: "none"},
		RelativePath:   "temp/" + partialKey + ".part",
		Size:           20,
		TotalBytes:     100,
		Ranges:         []model.ByteRange{{Start: 0, End: 20}},
		CreatedAt:      now.Add(-26 * time.Hour),
		LastAccessedAt: now.Add(-25 * time.Hour),
	}
	if err := index.Upsert(partialKey, partial); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	index = openIndex(t, root)
	defer index.Close()
	result, err := index.CleanTo(100, map[string]bool{newKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.BeforeBytes != 160 || result.AfterBytes != 80 {
		t.Fatalf("cleanup bytes: %#v", result)
	}
	wantDeleted := []string{partialKey, oldKey}
	if !reflect.DeepEqual(result.Deleted, wantDeleted) {
		t.Fatalf("deleted: got %#v want %#v", result.Deleted, wantDeleted)
	}
	for _, relative := range []string{"temp/" + partialKey + ".part", "audio/" + oldKey + ".mp3"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", relative, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "audio", newKey+".mp3")); err != nil {
		t.Fatalf("protected file removed: %v", err)
	}
	if _, ok, _ := index.Lookup(oldKey); ok {
		t.Fatal("old entry remains")
	}
	if _, ok, _ := index.Lookup(newKey); !ok {
		t.Fatal("protected entry missing")
	}
	if _, ok, _ := index.Lookup(partialKey); ok {
		t.Fatal("stale partial entry remains")
	}
}

func TestPruneStalePartialsHonorsProtectedAndMaxAge(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	staleKey := mustCacheKey(t, "0000000000000011", "128")
	protectedKey := mustCacheKey(t, "0000000000000012", "128")
	freshKey := mustCacheKey(t, "0000000000000013", "128")
	index := openIndex(t, root)
	defer index.Close()
	for _, item := range []struct {
		key string
		age time.Duration
	}{
		{staleKey, 26 * time.Hour},
		{protectedKey, 26 * time.Hour},
		{freshKey, time.Hour},
	} {
		writeCacheFile(t, root, "temp/"+item.key+".part", 3)
		entry := Entry{
			Track:        model.TrackInfo{Key: item.key, Hash: item.key[:16], Quality: "128", Effect: "none"},
			RelativePath: "temp/" + item.key + ".part", Size: 3, TotalBytes: 10,
			CreatedAt: now.Add(-item.age), LastAccessedAt: now.Add(-item.age),
		}
		if err := index.Upsert(item.key, entry); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := index.PruneStalePartials(24*time.Hour, map[string]bool{protectedKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{staleKey}) {
		t.Fatalf("deleted: %#v", deleted)
	}
	for _, key := range []string{protectedKey, freshKey} {
		if _, ok, _ := index.Lookup(key); !ok {
			t.Fatalf("retained partial %s missing", key)
		}
	}
}

func TestCleanToReturnsWithoutCompleteDeletionAtLimit(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, "0000000000000021", "high")
	writeCacheFile(t, root, "audio/"+key+".flac", 100)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, "1000000000000021", "high", "flac", 100, time.Now())); err != nil {
		t.Fatal(err)
	}
	result, err := index.CleanTo(100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BeforeBytes != 100 || result.AfterBytes != 100 || len(result.Deleted) != 0 {
		t.Fatalf("unexpected cleanup: %#v", result)
	}
}

func TestCleanToReachesNinetyPercentLowWaterMark(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	items := []struct {
		hash string
		size int
	}{
		{"0000000000000031", 6},
		{"0000000000000032", 6},
		{"0000000000000033", 6},
		{"0000000000000034", 87},
	}
	index := openIndex(t, root)
	defer index.Close()
	wantDeleted := make([]string, 0, 3)
	for position, item := range items {
		key := mustCacheKey(t, item.hash, "320")
		writeCacheFile(t, root, "audio/"+key+".mp3", item.size)
		entry := completeEntry(key, item.hash, "320", "mp3", int64(item.size), now.Add(time.Duration(position)*time.Minute))
		if err := index.Upsert(key, entry); err != nil {
			t.Fatal(err)
		}
		if position < 3 {
			wantDeleted = append(wantDeleted, key)
		}
	}
	result, err := index.CleanTo(100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BeforeBytes != 105 || result.AfterBytes != 87 {
		t.Fatalf("cleanup bytes: %#v", result)
	}
	if !reflect.DeepEqual(result.Deleted, wantDeleted) {
		t.Fatalf("deleted: got %#v want %#v", result.Deleted, wantDeleted)
	}
}

func TestCleanToReportsDeletionWhenPersistenceFails(t *testing.T) {
	root := t.TempDir()
	key := mustCacheKey(t, "0000000000000041", "320")
	writeCacheFile(t, root, "audio/"+key+".mp3", 120)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(key, completeEntry(key, "1000000000000041", "320", "mp3", 120, time.Now())); err != nil {
		t.Fatal(err)
	}
	persistErr := errors.New("injected cleanup persistence failure")
	index.hooks = &indexHooks{persist: func(map[string]Entry) error { return persistErr }}
	result, err := index.CleanTo(100, nil)
	if !errors.Is(err, persistErr) {
		t.Fatalf("cleanup error = %v, want persistence error", err)
	}
	if !reflect.DeepEqual(result.Deleted, []string{key}) || result.BeforeBytes != 120 || result.AfterBytes != 0 {
		t.Fatalf("cleanup result did not report deleted file: %#v", result)
	}
	if _, ok := index.Snapshot()[key]; !ok {
		t.Fatal("failed cleanup persistence mutated in-memory entries")
	}
	if _, err := os.Stat(filepath.Join(root, "audio", key+".mp3")); !os.IsNotExist(err) {
		t.Fatalf("deleted cache file still exists: %v", err)
	}
}

func TestCleanToPreservesDeletionAndPersistenceErrors(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	firstKey := mustCacheKey(t, "0000000000000051", "320")
	secondKey := mustCacheKey(t, "0000000000000052", "320")
	writeCacheFile(t, root, "audio/"+firstKey+".mp3", 10)
	writeCacheFile(t, root, "audio/"+secondKey+".mp3", 100)
	index := openIndex(t, root)
	defer index.Close()
	if err := index.Upsert(firstKey, completeEntry(firstKey, "1000000000000051", "320", "mp3", 10, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert(secondKey, completeEntry(secondKey, "1000000000000052", "320", "mp3", 100, now)); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("injected file deletion failure")
	persistErr := errors.New("injected cleanup persistence failure")
	index.hooks = &indexHooks{
		removeFile: func(relative string) error {
			if relative == "audio/"+firstKey+".mp3" {
				return os.Remove(filepath.Join(root, filepath.FromSlash(relative)))
			}
			return deleteErr
		},
		persist: func(map[string]Entry) error { return persistErr },
	}
	result, err := index.CleanTo(100, nil)
	if !errors.Is(err, deleteErr) || !errors.Is(err, persistErr) {
		t.Fatalf("cleanup error = %v, want deletion and persistence errors", err)
	}
	if !reflect.DeepEqual(result.Deleted, []string{firstKey}) || result.BeforeBytes != 110 || result.AfterBytes != 100 {
		t.Fatalf("cleanup result: %#v", result)
	}
	if len(index.Snapshot()) != 2 {
		t.Fatal("failed cleanup persistence mutated in-memory entries")
	}
}
