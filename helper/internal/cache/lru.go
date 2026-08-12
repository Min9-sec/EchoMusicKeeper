package cache

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

func (index *Index) PruneStalePartials(maxAge time.Duration, protected map[string]bool) ([]string, error) {
	index.mu.Lock()
	defer index.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	type candidate struct {
		key   string
		entry Entry
		at    time.Time
	}
	candidates := make([]candidate, 0)
	for key, entry := range index.entries {
		if entry.Complete || protected[key] {
			continue
		}
		at := entry.LastAccessedAt
		if at.IsZero() {
			at = entry.CreatedAt
		}
		if at.Before(cutoff) {
			candidates = append(candidates, candidate{key: key, entry: entry, at: at})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].at.Before(candidates[j].at)
	})

	next := cloneEntries(index.entries)
	deleted := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if err := index.removeFileLocked(candidate.entry.RelativePath); err != nil {
			persistErr := index.commitCleanupLocked(next, deleted)
			return deleted, errors.Join(err, persistErr)
		}
		delete(next, candidate.key)
		deleted = append(deleted, candidate.key)
	}
	if err := index.commitCleanupLocked(next, deleted); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (index *Index) CleanTo(limitBytes int64, protected map[string]bool) (CleanupResult, error) {
	deleted, err := index.PruneStalePartials(24*time.Hour, protected)
	result := CleanupResult{Deleted: append([]string(nil), deleted...)}
	if err != nil {
		index.mu.RLock()
		for _, entry := range index.entries {
			if entry.Complete {
				result.BeforeBytes += entry.Size
			}
		}
		index.mu.RUnlock()
		result.AfterBytes = result.BeforeBytes
		return result, err
	}
	index.mu.Lock()
	defer index.mu.Unlock()

	type candidate struct {
		key   string
		entry Entry
	}
	candidates := make([]candidate, 0)
	for key, entry := range index.entries {
		if !entry.Complete {
			continue
		}
		result.BeforeBytes += entry.Size
		if !protected[key] {
			candidates = append(candidates, candidate{key: key, entry: entry})
		}
	}
	result.AfterBytes = result.BeforeBytes
	if limitBytes < 0 {
		limitBytes = 0
	}
	if result.BeforeBytes <= limitBytes {
		return result, nil
	}
	target := lowWaterMark(limitBytes)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].entry.LastAccessedAt.Equal(candidates[j].entry.LastAccessedAt) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].entry.LastAccessedAt.Before(candidates[j].entry.LastAccessedAt)
	})

	next := cloneEntries(index.entries)
	completeDeleted := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if result.AfterBytes <= target {
			break
		}
		if err := index.removeFileLocked(candidate.entry.RelativePath); err != nil {
			result.Deleted = append(result.Deleted, completeDeleted...)
			persistErr := index.commitCleanupLocked(next, completeDeleted)
			return result, errors.Join(err, persistErr)
		}
		delete(next, candidate.key)
		completeDeleted = append(completeDeleted, candidate.key)
		result.AfterBytes -= candidate.entry.Size
		if result.AfterBytes < 0 {
			result.AfterBytes = 0
		}
	}
	result.Deleted = append(result.Deleted, completeDeleted...)
	if err := index.commitCleanupLocked(next, completeDeleted); err != nil {
		return result, fmt.Errorf("persist cache cleanup: %w", err)
	}
	return result, nil
}

// DeleteControlled removes one indexed file and its metadata. Callers own
// active-reference synchronization and must not invoke it for a protected key.
func (index *Index) DeleteControlled(key string) (bool, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	entry, found := index.entries[key]
	if !found {
		return false, nil
	}
	if err := index.removeFileLocked(entry.RelativePath); err != nil {
		return false, err
	}
	next := cloneEntries(index.entries)
	delete(next, key)
	if err := index.commitCleanupLocked(next, []string{key}); err != nil {
		return false, err
	}
	return true, nil
}

// ClearControlled removes every indexed file except keys protected by the
// manager. Both complete and partial entries are included.
func (index *Index) ClearControlled(protected map[string]bool) (CleanupResult, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	result := CleanupResult{}
	keys := sortedKeys(index.entries)
	next := cloneEntries(index.entries)
	for _, entry := range index.entries {
		result.BeforeBytes += entry.Size
	}
	result.AfterBytes = result.BeforeBytes
	for _, key := range keys {
		if protected[key] {
			continue
		}
		entry := next[key]
		if err := index.removeFileLocked(entry.RelativePath); err != nil {
			persistErr := index.commitCleanupLocked(next, result.Deleted)
			return result, errors.Join(err, persistErr)
		}
		delete(next, key)
		result.Deleted = append(result.Deleted, key)
		result.AfterBytes -= entry.Size
		if result.AfterBytes < 0 {
			result.AfterBytes = 0
		}
	}
	if err := index.commitCleanupLocked(next, result.Deleted); err != nil {
		return result, err
	}
	return result, nil
}

func (index *Index) commitCleanupLocked(entries map[string]Entry, deleted []string) error {
	if len(deleted) == 0 {
		return nil
	}
	if err := index.persistLocked(entries); err != nil {
		return err
	}
	index.entries = entries
	return nil
}

func lowWaterMark(limitBytes int64) int64 {
	return (limitBytes/10)*9 + ((limitBytes%10)*9)/10
}
