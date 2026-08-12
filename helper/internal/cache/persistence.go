package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func (index *Index) load() error {
	data, err := index.readRootFile(indexFilename)
	if errors.Is(err, fs.ErrNotExist) {
		if backupExists, backupErr := index.hasCorruptBackup(); backupErr != nil {
			return backupErr
		} else if backupExists {
			entries, scanErr := index.scanAudioForRecovery()
			if scanErr != nil {
				return fmt.Errorf("retry cache index recovery: %w", scanErr)
			}
			if err := index.persistLocked(entries); err != nil {
				return err
			}
			index.entries = entries
			return nil
		}
		return index.persistLocked(index.entries)
	}
	if err != nil {
		return fmt.Errorf("read cache index: %w", err)
	}
	var stored map[string]Entry
	if err := json.Unmarshal(data, &stored); err != nil {
		return index.recoverCorrupt()
	}
	changed := stored == nil
	if stored == nil {
		stored = make(map[string]Entry)
	}
	filtered := make(map[string]Entry, len(stored))
	for key, entry := range stored {
		normalized, normalizeErr := normalizeEntry(key, entry)
		if normalizeErr != nil {
			changed = true
			continue
		}
		stale, validationErr := index.validateStoredFile(normalized)
		if validationErr != nil {
			return fmt.Errorf("validate cache entry %q: %w", key, validationErr)
		}
		if stale {
			changed = true
			continue
		}
		if normalized.Track != entry.Track || normalized.AliasKey != entry.AliasKey {
			changed = true
		}
		filtered[key] = cloneEntry(normalized)
	}
	if changed || len(filtered) != len(stored) {
		if err := index.persistLocked(filtered); err != nil {
			return err
		}
	}
	index.entries = filtered
	return nil
}

func (index *Index) loadExisting() error {
	data, err := index.readRootFile(indexFilename)
	if err != nil {
		return fmt.Errorf("read existing cache index: %w", err)
	}
	var stored map[string]Entry
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode existing cache index: %w", err)
	}
	if stored == nil {
		stored = make(map[string]Entry)
	}
	validated := make(map[string]Entry, len(stored))
	for key, entry := range stored {
		normalized, err := normalizeEntry(key, entry)
		if err != nil {
			return fmt.Errorf("validate existing cache entry %q: %w", key, err)
		}
		stale, err := index.validateStoredFile(normalized)
		if err != nil {
			return fmt.Errorf("validate existing cache entry %q: %w", key, err)
		}
		if stale || !reflect.DeepEqual(normalized, entry) {
			return fmt.Errorf("existing cache entry %q requires repair", key)
		}
		validated[key] = cloneEntry(entry)
	}
	index.entries = validated
	return nil
}

func (index *Index) recoverCorrupt() error {
	backupName := fmt.Sprintf("index.corrupt-%d.json", time.Now().UTC().UnixNano())
	if err := index.rootFS.Rename(indexFilename, backupName); err != nil {
		return fmt.Errorf("back up corrupt cache index: %w", err)
	}
	entries, err := index.scanAudioForRecovery()
	if err != nil {
		restoreErr := index.restoreCorruptIndex(backupName)
		return errors.Join(err, restoreErr)
	}
	if err := index.persistLocked(entries); err != nil {
		restoreErr := index.restoreCorruptIndex(backupName)
		return errors.Join(fmt.Errorf("persist rebuilt cache index: %w", err), restoreErr)
	}
	index.entries = entries
	return nil
}

func (index *Index) scanAudioForRecovery() (map[string]Entry, error) {
	if index.hooks != nil && index.hooks.scanAudio != nil {
		return index.hooks.scanAudio()
	}
	return index.scanAudio()
}

func (index *Index) restoreCorruptIndex(backupName string) error {
	if info, err := index.rootFS.Lstat(indexFilename); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot restore corrupt index over non-regular index path")
		}
		if err := index.rootFS.Remove(indexFilename); err != nil {
			return fmt.Errorf("remove failed rebuilt index before restore: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect failed rebuilt index before restore: %w", err)
	}
	if err := index.rootFS.Rename(backupName, indexFilename); err != nil {
		return fmt.Errorf("restore corrupt cache index: %w", err)
	}
	return nil
}

func (index *Index) hasCorruptBackup() (bool, error) {
	directory, err := index.rootFS.Open(".")
	if err != nil {
		return false, fmt.Errorf("inspect cache root for corrupt backup: %w", err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return false, fmt.Errorf("list cache root for corrupt backup: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "index.corrupt-") && strings.HasSuffix(entry.Name(), ".json") {
			info, statErr := index.rootFS.Lstat(entry.Name())
			if statErr != nil {
				return false, fmt.Errorf("inspect corrupt index backup: %w", statErr)
			}
			if info.Mode().IsRegular() {
				return true, nil
			}
		}
	}
	return false, nil
}

func (index *Index) scanAudio() (map[string]Entry, error) {
	directory, err := index.audioFS.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open audio cache for recovery: %w", err)
	}
	defer directory.Close()
	directoryEntries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("scan audio cache for recovery: %w", err)
	}
	entries := make(map[string]Entry)
	for _, directoryEntry := range directoryEntries {
		if directoryEntry.Type()&os.ModeSymlink != 0 || directoryEntry.IsDir() {
			continue
		}
		key, extension, err := parseAudioBasename(directoryEntry.Name())
		if err != nil {
			continue
		}
		info, err := index.audioFS.Lstat(directoryEntry.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect audio cache file %q during recovery: %w", directoryEntry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		hash, quality, effect, _ := parseCacheKey(key)
		entry := Entry{
			Track: model.TrackInfo{
				Key: key, Hash: hash, Quality: quality, Effect: effect, Extension: extension,
			},
			RelativePath:   "audio/" + directoryEntry.Name(),
			Size:           info.Size(),
			TotalBytes:     info.Size(),
			Ranges:         []model.ByteRange{{Start: 0, End: info.Size()}},
			Complete:       true,
			CreatedAt:      info.ModTime(),
			CompletedAt:    info.ModTime(),
			LastAccessedAt: info.ModTime(),
		}
		entries[key] = entry
	}
	return entries, nil
}

func (index *Index) persistLocked(entries map[string]Entry) error {
	if index.hooks != nil && index.hooks.persist != nil {
		return index.hooks.persist(cloneEntries(entries))
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache index: %w", err)
	}
	data = append(data, '\n')
	if index.rootFS == nil {
		return fmt.Errorf("cache index is closed")
	}
	if info, statErr := index.rootFS.Lstat(indexTempFilename); statErr == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("temporary cache index is not a regular file")
	} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect temporary cache index: %w", statErr)
	}
	temporary, err := openRootFile(index.rootFS, indexTempFilename, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary cache index: %w", err)
	}
	cleanup := func() {
		_ = temporary.Close()
		_ = index.rootFS.Remove(indexTempFilename)
	}
	openedInfo, err := temporary.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		cleanup()
		return fmt.Errorf("temporary cache index is not a regular file")
	}
	currentInfo, err := index.rootFS.Lstat(indexTempFilename)
	if err != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(currentInfo, openedInfo) {
		cleanup()
		return fmt.Errorf("temporary cache index changed while it was opened")
	}
	if err := temporary.Truncate(0); err != nil {
		cleanup()
		return fmt.Errorf("truncate temporary cache index: %w", err)
	}
	written, err := temporary.Write(data)
	if err != nil {
		cleanup()
		return fmt.Errorf("write temporary cache index: %w", err)
	}
	if written != len(data) {
		cleanup()
		return fmt.Errorf("short write to temporary cache index: wrote %d of %d bytes", written, len(data))
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary cache index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = index.rootFS.Remove(indexTempFilename)
		return fmt.Errorf("close temporary cache index: %w", err)
	}
	if err := replaceFile(index.rootFS, index.root, indexTempFilename, indexFilename); err != nil {
		_ = index.rootFS.Remove(indexTempFilename)
		return fmt.Errorf("replace cache index: %w", err)
	}
	return nil
}

func (index *Index) readRootFile(name string) ([]byte, error) {
	info, err := index.rootFS.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cache index is not a regular file")
	}
	file, err := openRootFile(index.rootFS, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("cache index changed while it was opened")
	}
	return io.ReadAll(file)
}
