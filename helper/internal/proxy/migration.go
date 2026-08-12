package proxy

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

type migrationOperations struct {
	createDirectory func(string) (*security.Directory, error)
	openIndex       func(string) (*cache.Index, error)
	openExisting    func(string) (*cache.Index, error)
	rename          func(string, string) error
	removeEmpty     func(string, func()) (bool, error)
	cleanupStaging  func(*security.Directory, func()) (bool, error)
	sameVolume      func(string, string) bool
}

func defaultMigrationOperations() migrationOperations {
	return migrationOperations{
		createDirectory: security.CreateDirectory,
		openIndex:       cache.Open,
		openExisting:    cache.OpenExisting,
		rename:          anchoredRename,
		removeEmpty:     anchoredRemoveEmpty,
		cleanupStaging:  removeMigrationStaging,
		sameVolume:      sameWindowsVolume,
	}
}

func anchoredRename(oldPath, newPath string) error {
	anchor, oldRelative, err := security.OpenVolume(oldPath)
	if err != nil {
		return err
	}
	defer anchor.Close()
	newRelative, err := anchor.Relative(newPath)
	if err != nil {
		return err
	}
	return anchor.Rename(oldRelative, newRelative)
}

func anchoredRemoveEmpty(path string, afterCheck func()) (bool, error) {
	directory, err := security.OpenDirectory(path, false)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(".")
	if err != nil {
		return false, err
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("directory is not empty")
	}
	return directory.RemoveOpenedEmpty(afterCheck)
}

func openMigrationSource(index *cache.Index) (*security.Directory, error) {
	expected, err := index.RootIdentity()
	if err != nil {
		return nil, fmt.Errorf("verify source cache root: %w", err)
	}
	directory, err := security.OpenDirectory(index.Root(), false)
	if err != nil {
		return nil, fmt.Errorf("open source cache root: %w", err)
	}
	if !os.SameFile(expected, directory.Identity()) {
		_ = directory.Close()
		return nil, fmt.Errorf("source cache root identity changed")
	}
	return directory, nil
}

func removeMigrationSource(directory *security.Directory, afterCheck func()) (bool, error) {
	if directory == nil {
		return false, fmt.Errorf("opened source cache root is required")
	}
	if err := directory.CleanupContents(); err != nil {
		return false, err
	}
	return directory.RemoveOpenedEmpty(afterCheck)
}

func removeMigrationStaging(directory *security.Directory, afterCheck func()) (bool, error) {
	if directory == nil {
		return false, fmt.Errorf("opened staging cache root is required")
	}
	if err := directory.CleanupContents(); err != nil {
		return false, err
	}
	return directory.RemoveOpenedEmpty(afterCheck)
}

type migrationSnapshot struct {
	entries map[string]cache.Entry
	hashes  map[string][sha256.Size]byte
}

type migrationLease struct {
	manager  *Manager
	index    *cache.Index
	snapshot migrationSnapshot
	source   *security.Directory
	sessions []*Session
	done     bool
}

func (manager *Manager) beginMigration() (*migrationLease, error) {
	for {
		manager.mu.Lock()
		if manager.closed {
			manager.mu.Unlock()
			return nil, fmt.Errorf("proxy manager is closed")
		}
		if manager.unavailableErr != nil {
			err := manager.unavailableErr
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache manager unavailable: %w", err)
		}
		if manager.index == nil {
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache index is required")
		}
		if manager.migrating {
			manager.mu.Unlock()
			return nil, fmt.Errorf("cache migration is already running")
		}
		if manager.cleaning {
			done := manager.cleaningDone
			manager.mu.Unlock()
			<-done
			continue
		}
		manager.migrating = true
		manager.migrationDone = make(chan struct{})
		for len(manager.references) != 0 {
			manager.condition.Wait()
		}
		sessions := make([]*Session, 0, len(manager.sessions))
		for _, session := range manager.sessions {
			session.mu.Lock()
			session.migrations++
			session.mu.Unlock()
			sessions = append(sessions, session)
		}
		lease := &migrationLease{manager: manager, index: manager.index, sessions: sessions}
		manager.mu.Unlock()
		return lease, nil
	}
}

func (lease *migrationLease) prepare() error {
	source, err := openMigrationSource(lease.index)
	if err != nil {
		return err
	}
	snapshot, err := captureMigrationSnapshot(lease.index)
	if err != nil {
		_ = source.Close()
		return err
	}
	lease.source = source
	lease.snapshot = snapshot
	return nil
}

func (lease *migrationLease) closeCapabilities() error {
	if lease.source == nil {
		return nil
	}
	err := lease.source.Close()
	lease.source = nil
	return err
}

func captureMigrationSnapshot(index *cache.Index) (migrationSnapshot, error) {
	snapshot := migrationSnapshot{entries: index.Snapshot(), hashes: make(map[string][sha256.Size]byte)}
	for key, entry := range snapshot.entries {
		file, err := index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
		if err != nil {
			return migrationSnapshot{}, fmt.Errorf("open cache snapshot entry %q: %w", key, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return migrationSnapshot{}, fmt.Errorf("hash cache snapshot entry %q: %w", key, err)
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		snapshot.hashes[key] = digest
	}
	return snapshot, nil
}

func (lease *migrationLease) finish(selected *cache.Index, keepSessions bool) {
	if lease.done {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	if selected != nil {
		manager.index = selected
		manager.unavailableErr = nil
		if !keepSessions {
			manager.sessions = make(map[string]*Session)
		}
	}
	for _, session := range lease.sessions {
		session.mu.Lock()
		if selected != nil && keepSessions {
			session.index = selected
		}
		if session.migrations > 0 {
			session.migrations--
		}
		session.mu.Unlock()
	}
	manager.migrating = false
	close(manager.migrationDone)
	manager.migrationDone = nil
	manager.condition.Broadcast()
	manager.mu.Unlock()
	lease.done = true
	for _, session := range lease.sessions {
		manager.expireSession(session.key, session)
	}
}

func (lease *migrationLease) finishUnavailable(reason error) {
	if lease.done {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	manager.index = nil
	manager.unavailableErr = reason
	for _, session := range lease.sessions {
		session.mu.Lock()
		if session.migrations > 0 {
			session.migrations--
		}
		session.mu.Unlock()
	}
	manager.migrating = false
	close(manager.migrationDone)
	manager.migrationDone = nil
	manager.condition.Broadcast()
	manager.mu.Unlock()
	lease.done = true
}

func (manager *Manager) SwitchCacheRoot(newRoot string) (resultErr error) {
	lease, err := manager.beginMigration()
	if err != nil {
		return err
	}
	if err := lease.prepare(); err != nil {
		lease.finish(nil, true)
		return err
	}
	defer func() {
		if !lease.done {
			lease.finish(nil, true)
		}
		resultErr = errors.Join(resultErr, lease.closeCapabilities(), manager.cleanCache())
	}()
	oldRoot := lease.index.Root()
	destination, _, err := validateEmptyCacheDestination(newRoot, oldRoot)
	if err != nil {
		return err
	}
	selected, err := manager.migrationOps.openIndex(destination)
	if err != nil {
		return fmt.Errorf("open fresh cache root: %w", err)
	}
	if len(selected.Snapshot()) != 0 {
		_ = selected.Close()
		return fmt.Errorf("fresh cache root is not empty")
	}
	for _, session := range lease.sessions {
		resultErr = errors.Join(resultErr, session.close(false))
	}
	if resultErr != nil {
		_ = selected.Close()
		return resultErr
	}
	if err := lease.index.Close(); err != nil {
		_ = selected.Close()
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: lease.source.Identity()},
		}, fmt.Errorf("close old cache root: %w", err))
	}
	lease.finish(selected, false)
	return nil
}

func (manager *Manager) MigrateCache(newRoot string, deleteOld bool) (resultErr error) {
	lease, err := manager.beginMigration()
	if err != nil {
		return err
	}
	if err := lease.prepare(); err != nil {
		lease.finish(nil, true)
		return err
	}
	defer func() {
		if !lease.done {
			lease.finish(nil, true)
		}
		resultErr = errors.Join(resultErr, lease.closeCapabilities(), manager.cleanCache())
	}()
	oldRoot := lease.index.Root()
	destination, destinationExists, err := validateEmptyCacheDestination(newRoot, oldRoot)
	if err != nil {
		return err
	}
	sameVolume := deleteOld && manager.migrationOps.sameVolume(oldRoot, destination)
	if sameVolume {
		return manager.renameCacheRoot(lease, oldRoot, destination, destinationExists)
	}
	return manager.copyCacheRoot(lease, oldRoot, destination, destinationExists, deleteOld)
}

func (manager *Manager) renameCacheRoot(lease *migrationLease, oldRoot, destination string, destinationExists bool) (resultErr error) {
	probe, probeDirectory, err := manager.createStagingIndex(destination)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, probeDirectory.Close())
	}()
	if err := probe.Close(); err != nil {
		_, cleanupErr := manager.cleanupStaging(probeDirectory)
		return errors.Join(err, cleanupErr)
	}
	if _, err := manager.cleanupStaging(probeDirectory); err != nil {
		return fmt.Errorf("remove migration probe: %w", err)
	}
	if destinationExists {
		removed, err := manager.migrationOps.removeEmpty(destination, manager.afterDestinationCheck)
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("same-volume migration cannot safely remove an existing empty destination on this platform")
		}
	}
	if err := lease.index.Close(); err != nil {
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: lease.source.Identity()},
		}, fmt.Errorf("close source cache before move: %w", err))
	}
	if err := manager.migrationOps.rename(oldRoot, destination); err != nil {
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: lease.source.Identity()},
		}, fmt.Errorf("move cache root: %w", err))
	}
	identity := lease.source.Identity()
	selected, err := manager.openMigrationCandidate(destination, identity, lease.snapshot)
	if err != nil {
		rollbackErr := manager.migrationOps.rename(destination, oldRoot)
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: identity},
			{path: destination, identity: identity},
		}, errors.Join(fmt.Errorf("open moved cache root: %w", err), rollbackErr))
	}
	lease.finish(selected, true)
	return nil
}

func (manager *Manager) copyCacheRoot(lease *migrationLease, oldRoot, destination string, destinationExists, deleteOld bool) (resultErr error) {
	if destinationExists {
		removed, err := manager.migrationOps.removeEmpty(destination, manager.afterDestinationCheck)
		if err != nil {
			return err
		}
		if !removed {
			return manager.copyCacheIntoExisting(lease, oldRoot, destination, deleteOld)
		}
	}
	staging, stagingDirectory, err := manager.createStagingIndex(destination)
	if err != nil {
		return err
	}
	stagingRoot := staging.Root()
	cleanupStaging := true
	defer func() {
		resultErr = errors.Join(resultErr, staging.Close())
		if cleanupStaging {
			if manager.beforeStagingCleanup != nil {
				manager.beforeStagingCleanup(stagingRoot)
			}
			removed, err := manager.cleanupStaging(stagingDirectory)
			if err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("staging cleanup: %w", err))
			}
			resultErr = errors.Join(resultErr, retainedEmptyError("staging cleanup", removed))
		}
		resultErr = errors.Join(resultErr, stagingDirectory.Close())
	}()
	if err := lease.index.CopyTo(staging); err != nil {
		return fmt.Errorf("copy cache into staging: %w", err)
	}
	if err := staging.Close(); err != nil {
		return fmt.Errorf("close staged cache index: %w", err)
	}
	reopened, err := manager.openMigrationCandidate(stagingRoot, stagingDirectory.Identity(), lease.snapshot)
	if err != nil {
		return fmt.Errorf("reopen staged cache index: %w", err)
	}
	if err := reopened.Close(); err != nil {
		return fmt.Errorf("close validated staged cache index: %w", err)
	}
	if err := manager.migrationOps.rename(stagingRoot, destination); err != nil {
		return fmt.Errorf("publish migrated cache root: %w", err)
	}
	cleanupStaging = false
	destinationIdentity := stagingDirectory.Identity()
	selected, err := manager.openMigrationCandidate(destination, destinationIdentity, lease.snapshot)
	if err != nil {
		rollbackErr := manager.migrationOps.rename(destination, stagingRoot)
		if rollbackErr == nil {
			cleanupStaging = true
			return fmt.Errorf("open migrated cache root: %w", err)
		}
		cleanupStaging = false
		cleanupErr := cleanupPublishedMigration(stagingDirectory, manager.afterPublishedCleanup)
		return errors.Join(
			fmt.Errorf("open migrated cache root: %w", err),
			fmt.Errorf("roll back migrated cache root: %w", rollbackErr),
			wrapError("published destination cleanup", cleanupErr),
		)
	}
	if err := lease.index.Close(); err != nil {
		_ = selected.Close()
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: lease.source.Identity()},
			{path: destination, identity: destinationIdentity},
		}, fmt.Errorf("close old cache root: %w", err))
	}
	lease.finish(selected, true)
	if deleteOld {
		if manager.beforeOldRootDelete != nil {
			manager.beforeOldRootDelete()
		}
		removed, err := removeMigrationSource(lease.source, manager.afterOldRootCleanup)
		if err != nil {
			return fmt.Errorf("remove old cache root: %w", err)
		}
		if !removed {
			return fmt.Errorf("old cache contents removed; empty root retained on this platform")
		}
	}
	return nil
}

func (manager *Manager) copyCacheIntoExisting(lease *migrationLease, oldRoot, destination string, deleteOld bool) (resultErr error) {
	directory, err := security.OpenDirectory(destination, false)
	if err != nil {
		return fmt.Errorf("open retained empty cache destination: %w", err)
	}
	cleanupDestination := true
	var destinationIndex *cache.Index
	defer func() {
		if destinationIndex != nil {
			resultErr = errors.Join(resultErr, destinationIndex.Close())
		}
		if cleanupDestination {
			resultErr = errors.Join(resultErr, wrapError("retained destination cleanup", directory.CleanupContents()))
		}
		resultErr = errors.Join(resultErr, directory.Close())
	}()
	root, err := directory.CloneRoot()
	if err != nil {
		return fmt.Errorf("clone retained cache destination: %w", err)
	}
	if manager.afterDestinationClone != nil {
		manager.afterDestinationClone()
	}
	destinationIndex, err = cache.OpenAnchored(destination, root)
	if err != nil {
		return fmt.Errorf("initialize retained cache destination: %w", err)
	}
	if err := lease.index.CopyTo(destinationIndex); err != nil {
		return fmt.Errorf("copy cache into retained destination: %w", err)
	}
	if err := destinationIndex.Close(); err != nil {
		return fmt.Errorf("close retained cache destination: %w", err)
	}
	selected, err := manager.openMigrationCandidate(destination, directory.Identity(), lease.snapshot)
	if err != nil {
		return fmt.Errorf("open retained migrated cache root: %w", err)
	}
	if err := lease.index.Close(); err != nil {
		_ = selected.Close()
		return manager.recoverMigrationIndex(lease, []recoveryCandidate{
			{path: oldRoot, identity: lease.source.Identity()},
			{path: destination, identity: directory.Identity()},
		}, fmt.Errorf("close old cache root: %w", err))
	}
	lease.finish(selected, true)
	cleanupDestination = false
	if deleteOld {
		if manager.beforeOldRootDelete != nil {
			manager.beforeOldRootDelete()
		}
		removed, err := removeMigrationSource(lease.source, manager.afterOldRootCleanup)
		if err != nil {
			return fmt.Errorf("remove old cache root: %w", err)
		}
		if !removed {
			return fmt.Errorf("old cache contents removed; empty root retained on this platform")
		}
	}
	return nil
}

func cleanupPublishedMigration(directory *security.Directory, afterCleanup func()) error {
	if directory == nil {
		return fmt.Errorf("opened published cache root is required")
	}
	if err := directory.CleanupContents(); err != nil {
		return fmt.Errorf("clean published cache contents: %w", err)
	}
	if afterCleanup != nil {
		afterCleanup()
	}
	return nil
}

type recoveryCandidate struct {
	path     string
	identity os.FileInfo
}

func (manager *Manager) recoverMigrationIndex(lease *migrationLease, candidates []recoveryCandidate, primary error) error {
	var recoveryErr error
	for _, candidate := range candidates {
		index, err := manager.openMigrationCandidate(candidate.path, candidate.identity, lease.snapshot)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("reopen cache root %q: %w", candidate.path, err))
			continue
		}
		lease.finish(index, true)
		return errors.Join(primary, recoveryErr)
	}
	reason := errors.Join(primary, recoveryErr)
	lease.finishUnavailable(reason)
	return reason
}

func (manager *Manager) openMigrationCandidate(path string, expected os.FileInfo, snapshot migrationSnapshot) (*cache.Index, error) {
	if expected == nil {
		return nil, fmt.Errorf("expected cache root identity is required")
	}
	directory, err := security.OpenDirectory(path, false)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, directory.Identity()) {
		_ = directory.Close()
		return nil, fmt.Errorf("cache root identity changed")
	}
	defer directory.Close()
	index, err := manager.migrationOps.openExisting(path)
	if err != nil {
		return nil, err
	}
	identity, err := index.RootIdentity()
	if err != nil || !os.SameFile(expected, identity) {
		_ = index.Close()
		return nil, fmt.Errorf("opened cache root identity changed")
	}
	if err := validateMigratedIndex(snapshot, index); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

func (manager *Manager) cleanupStaging(directory *security.Directory) (bool, error) {
	if directory == nil {
		return false, fmt.Errorf("opened staging directory is required")
	}
	return manager.migrationOps.cleanupStaging(directory, manager.afterStagingCleanup)
}

func validateEmptyCacheDestination(newRoot, oldRoot string) (string, bool, error) {
	if strings.TrimSpace(newRoot) == "" {
		return "", false, fmt.Errorf("new cache root is required")
	}
	destination, err := filepath.Abs(filepath.Clean(newRoot))
	if err != nil {
		return "", false, fmt.Errorf("resolve new cache root: %w", err)
	}
	if samePath(destination, oldRoot) {
		return "", false, fmt.Errorf("new cache root must differ from the current root")
	}
	if pathsOverlap(destination, oldRoot) {
		return "", false, fmt.Errorf("cache roots must not contain each other")
	}
	anchor, relative, err := security.OpenVolume(destination)
	if err != nil {
		return "", false, fmt.Errorf("open new cache volume: %w", err)
	}
	defer anchor.Close()
	info, err := anchor.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return destination, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect new cache root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("new cache root is not a real directory")
	}
	directory, err := security.OpenDirectory(destination, false)
	if err != nil {
		return "", false, fmt.Errorf("open new cache root: %w", err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(".")
	if err != nil {
		return "", false, fmt.Errorf("inspect new cache root contents: %w", err)
	}
	if len(entries) != 0 {
		return "", false, fmt.Errorf("new cache root is not empty")
	}
	return destination, true, nil
}

func (manager *Manager) createStagingIndex(destination string) (*cache.Index, *security.Directory, error) {
	for attempt := 0; attempt < 8; attempt++ {
		suffix := "staging"
		if security.CanRemoveOpenedDirectory() {
			secret, err := newSecret()
			if err != nil {
				return nil, nil, err
			}
			suffix = secret[:12]
		} else if attempt > 0 {
			break
		}
		staging := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+".migration-"+suffix)
		directory, err := manager.migrationOps.createDirectory(staging)
		if errors.Is(err, fs.ErrExist) {
			if !security.CanRemoveOpenedDirectory() {
				return nil, nil, fmt.Errorf("cache migration staging root %q already exists; remove the retained empty directory before retry", staging)
			}
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("create cache migration staging root: %w", err)
		}
		index, err := manager.migrationOps.openIndex(staging)
		if err != nil {
			removed, cleanupErr := manager.cleanupStaging(directory)
			closeErr := directory.Close()
			return nil, nil, errors.Join(
				fmt.Errorf("open cache migration staging index: %w", err),
				wrapError("staging cleanup", cleanupErr), retainedEmptyError("staging cleanup", removed), closeErr,
			)
		}
		identity, err := index.RootIdentity()
		if err != nil || !os.SameFile(identity, directory.Identity()) {
			if err == nil {
				err = fmt.Errorf("staging cache root identity changed")
			}
			_ = index.Close()
			removed, cleanupErr := manager.cleanupStaging(directory)
			closeErr := directory.Close()
			return nil, nil, errors.Join(
				fmt.Errorf("anchor cache migration staging root: %w", err),
				wrapError("staging cleanup", cleanupErr), retainedEmptyError("staging cleanup", removed), closeErr,
			)
		}
		return index, directory, nil
	}
	return nil, nil, fmt.Errorf("allocate cache migration staging root")
}

func wrapError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}

func retainedEmptyError(label string, removed bool) error {
	if removed {
		return nil
	}
	return fmt.Errorf("%s retained an empty owned directory on this platform", label)
}

func validateMigratedIndex(expected migrationSnapshot, destination *cache.Index) error {
	actual := destination.Snapshot()
	if !reflect.DeepEqual(actual, expected.entries) {
		return fmt.Errorf("migrated cache index metadata changed")
	}
	for key, entry := range actual {
		file, err := destination.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("open migrated cache entry %q: %w", key, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return fmt.Errorf("hash migrated cache entry %q: %w", key, err)
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		if digest != expected.hashes[key] {
			return fmt.Errorf("migrated cache entry %q content changed", key)
		}
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != "." && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sameWindowsVolume(left, right string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	leftVolume := filepath.VolumeName(left)
	rightVolume := filepath.VolumeName(right)
	return leftVolume != "" && strings.EqualFold(leftVolume, rightVolume)
}
