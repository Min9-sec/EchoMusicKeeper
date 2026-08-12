package proxy

import (
	"errors"
	"fmt"
	"os"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func (session *Session) checkpointPartial() error {
	return session.checkpointPartialWithFile(nil, true)
}

func (session *Session) checkpointPartialIfDue(file *os.File) error {
	return session.checkpointPartialWithFile(file, false)
}

func (session *Session) checkpointPartialWithFile(file *os.File, force bool) error {
	session.checkpointMu.Lock()
	defer session.checkpointMu.Unlock()
	now := session.options.now().UTC()
	session.mu.Lock()
	if !force && session.bytesSinceCheckpoint < session.options.checkpointBytes && now.Sub(session.lastCheckpointAt) < session.options.checkpointInterval {
		session.mu.Unlock()
		return nil
	}
	entry := session.partialEntryLocked()
	session.mu.Unlock()
	var syncErr error
	if file != nil {
		syncErr = file.Sync()
	} else {
		opened, err := session.index.OpenFile(session.partPath, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr = errors.Join(opened.Sync(), opened.Close())
	}
	if syncErr != nil {
		return syncErr
	}
	if err := session.index.Upsert(session.key, entry); err != nil {
		return err
	}
	session.mu.Lock()
	session.bytesSinceCheckpoint = 0
	session.lastCheckpointAt = now
	session.mu.Unlock()
	return nil
}

func (session *Session) partialEntryLocked() cache.Entry {
	return cache.Entry{
		Track:          session.track,
		RelativePath:   session.partPath,
		Size:           coveredBytes(session.ranges),
		TotalBytes:     session.totalBytes,
		Ranges:         append([]model.ByteRange(nil), session.ranges...),
		CreatedAt:      session.createdAt,
		LastAccessedAt: session.updatedAt,
	}
}

func (session *Session) syncPartial() error {
	session.mu.Lock()
	complete := session.state == model.TaskCompleted
	path := session.partPath
	session.mu.Unlock()
	if complete {
		return nil
	}
	if _, err := session.index.StatFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return session.checkpointPartial()
}

func (session *Session) finalize() error {
	session.mu.Lock()
	if session.totalBytes <= 0 || len(cache.Missing(session.totalBytes, session.ranges)) != 0 {
		session.mu.Unlock()
		return fmt.Errorf("cache file is incomplete")
	}
	key, normalizedTrack, err := normalizeTrack(session.track)
	if err != nil || key != session.key {
		session.mu.Unlock()
		return fmt.Errorf("cache key changed before completion")
	}
	total := session.totalBytes
	partPath := session.partPath
	finalPath := "audio/" + key + "." + normalizedTrack.Extension
	createdAt := session.createdAt
	contentType := session.contentType
	session.mu.Unlock()

	if err := session.checkpointPartial(); err != nil {
		return err
	}
	if err := session.index.RenameFile(partPath, finalPath); err != nil {
		return err
	}
	session.setCurrentPath(finalPath)
	info, err := session.index.StatFile(finalPath)
	if err != nil || info.Size() != total {
		primary := err
		if err != nil {
			primary = fmt.Errorf("stat completed cache file: %w", err)
		} else {
			primary = fmt.Errorf("completed cache size does not match upstream length")
		}
		return session.restorePartialAfterRename(finalPath, partPath, primary)
	}
	now := session.options.now().UTC()
	entry := cache.Entry{
		Track:          normalizedTrack,
		RelativePath:   finalPath,
		Size:           info.Size(),
		TotalBytes:     total,
		Ranges:         []model.ByteRange{{Start: 0, End: total}},
		Complete:       true,
		CreatedAt:      createdAt,
		CompletedAt:    now,
		LastAccessedAt: now,
	}
	if err := session.commitCompleteEntry(entry); err != nil {
		return err
	}

	session.mu.Lock()
	session.track = normalizedTrack
	session.contentType = contentType
	session.currentPath = finalPath
	session.finalPath = finalPath
	session.ranges = entry.Ranges
	session.lastError = ""
	session.remoteURL = ""
	session.producer = false
	session.completedAt = now
	session.updatedAt = now
	canceled := session.stopped || session.state == model.TaskCanceled
	if !canceled {
		session.state = model.TaskCompleted
	}
	session.signalLocked()
	if !canceled {
		session.scheduleExpiryLocked()
	}
	session.mu.Unlock()
	return nil
}

func (session *Session) commitCompleteEntry(entry cache.Entry) error {
	if err := session.index.Upsert(session.key, entry); err != nil {
		rollbackErr := session.index.RenameFile(entry.RelativePath, session.partPath)
		if rollbackErr == nil {
			session.setCurrentPath(session.partPath)
			return err
		}

		finalInfo, finalStatErr := session.index.StatFile(entry.RelativePath)
		var repairErr error
		if finalStatErr == nil && finalInfo.Size() == entry.Size {
			repairErr = session.index.Upsert(session.key, entry)
			if repairErr == nil {
				if removeErr := session.index.RemoveFile(session.partPath); removeErr != nil {
					return errors.Join(err, rollbackErr, removeErr)
				}
				return nil
			}
		}

		partInfo, partStatErr := session.index.StatFile(session.partPath)
		if partStatErr == nil && partInfo.Mode().IsRegular() {
			session.setCurrentPath(session.partPath)
			removeErr := session.index.RemoveFile(entry.RelativePath)
			partialErr := session.index.Upsert(session.key, session.partialEntry())
			return errors.Join(err, rollbackErr, repairErr, unexpectedStatError(finalStatErr), removeErr, partialErr)
		}
		return errors.Join(err, rollbackErr, repairErr, finalStatErr, partStatErr)
	}
	return nil
}

func (session *Session) restorePartialAfterRename(finalPath, partPath string, primary error) error {
	rollbackErr := session.index.RenameFile(finalPath, partPath)
	if rollbackErr == nil {
		session.setCurrentPath(partPath)
		return primary
	}
	// A second rooted rename handles a transient rollback failure without
	// guessing at absolute paths or discarding the first error.
	retryErr := session.index.RenameFile(finalPath, partPath)
	if retryErr == nil {
		session.setCurrentPath(partPath)
		return errors.Join(primary, rollbackErr)
	}
	partInfo, partStatErr := session.index.StatFile(partPath)
	if partStatErr == nil && partInfo.Mode().IsRegular() {
		session.setCurrentPath(partPath)
		removeErr := session.index.RemoveFile(finalPath)
		partialErr := session.index.Upsert(session.key, session.partialEntry())
		return errors.Join(primary, rollbackErr, retryErr, removeErr, partialErr)
	}
	return errors.Join(primary, rollbackErr, retryErr, partStatErr)
}

func unexpectedStatError(err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (session *Session) partialEntry() cache.Entry {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.partialEntryLocked()
}
