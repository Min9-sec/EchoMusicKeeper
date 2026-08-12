package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

func (manager *Manager) CreateDownload(ctx context.Context, request model.CreateSessionRequest) (model.Task, error) {
	if err := ctx.Err(); err != nil {
		return model.Task{}, err
	}
	manager.mu.Lock()
	root := manager.downloadRoot
	manager.mu.Unlock()
	if root == "" {
		return model.Task{}, fmt.Errorf("download root is not configured")
	}
	session, err := manager.CreateOrReuse(ctx, request)
	if err != nil {
		return model.Task{}, err
	}
	return manager.ExportWhenComplete(session.Key(), root)
}

func (manager *Manager) ExportWhenComplete(key, downloadRoot string) (model.Task, error) {
	validatedKey, err := validateCacheKey(key)
	if err != nil {
		return model.Task{}, err
	}
	directory, err := security.OpenDirectory(downloadRoot, true)
	if err != nil {
		return model.Task{}, fmt.Errorf("open download root: %w", err)
	}
	release, err := manager.acquireReference(validatedKey, nil, referenceExport)
	if err != nil {
		_ = directory.Close()
		return model.Task{}, err
	}

	manager.mu.Lock()
	index := manager.index
	session := manager.sessions[validatedKey]
	manager.mu.Unlock()
	var track model.TrackInfo
	var total int64
	if session != nil {
		snapshot := session.task()
		track = snapshot.Track
		total = snapshot.TotalBytes
	} else if index != nil {
		entry, found, lookupErr := index.Lookup(validatedKey)
		if lookupErr != nil {
			release()
			_ = directory.Close()
			return model.Task{}, lookupErr
		}
		if !found || !entry.Complete {
			release()
			_ = directory.Close()
			return model.Task{}, fmt.Errorf("completed cache entry %q does not exist", validatedKey)
		}
		track = entry.Track
		total = entry.Size
	} else {
		release()
		_ = directory.Close()
		return model.Task{}, fmt.Errorf("cache index is required")
	}
	id, err := newSecret()
	if err != nil {
		release()
		_ = directory.Close()
		return model.Task{}, err
	}
	now := time.Now().UTC()
	download := &downloadTask{
		manager:     manager,
		key:         validatedKey,
		root:        directory.Path(),
		directory:   directory,
		session:     session,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
		initialized: make(chan struct{}),
		task: model.Task{
			ID: id, Kind: model.TaskKindDownload, Track: track, State: model.TaskQueued,
			TotalBytes: total, Pausable: true, Cancelable: true, UpdatedAt: now,
		},
	}
	download.ctx, download.cancel = context.WithCancel(context.Background())
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		release()
		_ = directory.Close()
		return model.Task{}, fmt.Errorf("proxy manager is closed")
	}
	manager.downloads[id] = download
	manager.mu.Unlock()
	go download.run(release)
	<-download.initialized
	return download.snapshot(), nil
}

func (manager *Manager) Pause(taskID string) error {
	download, err := manager.downloadTask(taskID)
	if err != nil {
		return err
	}
	return download.pause()
}

func (manager *Manager) Resume(taskID string) error {
	download, err := manager.downloadTask(taskID)
	if err != nil {
		return err
	}
	return download.resume()
}

func (manager *Manager) Retry(taskID string) error {
	download, err := manager.downloadTask(taskID)
	if err != nil {
		return err
	}
	return download.retry()
}

func (manager *Manager) downloadTask(taskID string) (*downloadTask, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if download := manager.downloads[taskID]; download != nil {
		return download, nil
	}
	return nil, fmt.Errorf("download task %q does not exist", taskID)
}

var errSourcePending = errors.New("cache source is not complete")

type downloadTask struct {
	mu sync.Mutex

	manager     *Manager
	key         string
	root        string
	directory   *security.Directory
	session     *Session
	task        model.Task
	ctx         context.Context
	cancel      context.CancelFunc
	wake        chan struct{}
	done        chan struct{}
	initialized chan struct{}
	generation  uint64
	attempt     *downloadAttempt
	cleanupErr  error
}

type downloadAttempt struct {
	generation uint64
	cancel     context.CancelFunc
}

func (download *downloadTask) snapshot() model.Task {
	download.mu.Lock()
	defer download.mu.Unlock()
	return download.task
}

func (download *downloadTask) run(initialRelease func()) {
	defer close(download.done)
	defer download.directory.Close()
	release := initialRelease
	initialized := false
	defer func() {
		if release != nil {
			release()
		}
		if !initialized {
			close(download.initialized)
		}
	}()
	for {
		generation, runnable := download.waitUntilRunnable()
		if !runnable {
			return
		}
		if release == nil {
			var err error
			release, err = download.manager.acquireReference(download.key, download.session, referenceExport)
			if err != nil {
				download.markCanceled()
				return
			}
		}
		attemptContext, cancel := context.WithCancel(download.ctx)
		if !download.beginAttempt(generation, cancel) {
			cancel()
			release()
			release = nil
			continue
		}
		if !initialized {
			close(download.initialized)
			initialized = true
		}
		index, entry, err := download.waitForSource(attemptContext)
		output := ""
		if err == nil {
			output, err = download.copy(attemptContext, index, entry, generation)
		}
		cancel()
		download.clearAttempt(generation)
		release()
		release = nil
		if download.finishAttempt(generation, output, err) {
			return
		}
	}
}

func (download *downloadTask) waitUntilRunnable() (uint64, bool) {
	for {
		download.mu.Lock()
		state := download.task.State
		generation := download.generation
		wake := download.wake
		download.mu.Unlock()
		if state == model.TaskCanceled || state == model.TaskCompleted {
			return 0, false
		}
		if state != model.TaskPaused && state != model.TaskFailed {
			return generation, true
		}
		select {
		case <-download.ctx.Done():
			download.markCanceled()
			return 0, false
		case <-wake:
		}
	}
}

func (download *downloadTask) beginAttempt(generation uint64, cancel context.CancelFunc) bool {
	download.mu.Lock()
	defer download.mu.Unlock()
	if generation != download.generation || download.task.State == model.TaskPaused || download.task.State == model.TaskFailed || download.task.State == model.TaskCanceled {
		return false
	}
	download.attempt = &downloadAttempt{generation: generation, cancel: cancel}
	download.task.State = model.TaskRunning
	download.task.Pausable = true
	download.task.Cancelable = true
	download.task.Retryable = false
	download.task.Error = download.cleanupErrorLocked()
	download.task.UpdatedAt = time.Now().UTC()
	return true
}

func (download *downloadTask) clearAttempt(generation uint64) {
	download.mu.Lock()
	if download.attempt != nil && download.attempt.generation == generation {
		download.attempt = nil
	}
	download.mu.Unlock()
}

func (download *downloadTask) waitForSource(ctx context.Context) (*cache.Index, cache.Entry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, cache.Entry{}, err
		}
		download.manager.mu.Lock()
		index := download.manager.index
		download.manager.mu.Unlock()
		if index == nil {
			return nil, cache.Entry{}, fmt.Errorf("cache index is required")
		}
		entry, found, lookupErr := index.Lookup(download.key)
		if lookupErr != nil {
			return nil, cache.Entry{}, lookupErr
		}
		if found && entry.Complete {
			return index, entry, nil
		}
		if download.session == nil {
			return nil, cache.Entry{}, fmt.Errorf("%w: %s", errSourcePending, download.key)
		}
		download.session.mu.Lock()
		state := download.session.state
		changed := download.session.changed
		download.session.mu.Unlock()
		switch state {
		case model.TaskCanceled:
			return nil, cache.Entry{}, fmt.Errorf("cache source was canceled")
		case model.TaskFailed, model.TaskNeedsURL:
			return nil, cache.Entry{}, fmt.Errorf("cache source is unavailable")
		}
		select {
		case <-ctx.Done():
			return nil, cache.Entry{}, ctx.Err()
		case <-changed:
		}
	}
}

func (download *downloadTask) copy(ctx context.Context, index *cache.Index, entry cache.Entry, generation uint64) (string, error) {
	input, err := index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("open completed cache file: %w", err)
	}
	info, statErr := input.Stat()
	if statErr != nil {
		_ = input.Close()
		return "", fmt.Errorf("inspect completed cache file: %w", statErr)
	}
	download.mu.Lock()
	if generation == download.generation {
		download.task.TotalBytes = info.Size()
		download.task.DownloadedBytes = 0
		download.task.UpdatedAt = time.Now().UTC()
	}
	download.mu.Unlock()

	progress := &downloadProgressReader{download: download, reader: input, generation: generation}
	output, copyErr := download.manager.copyDownload(ctx, progress, info.Size(), download.directory, entry.Track)
	closeErr := input.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		if output != "" {
			err = errors.Join(err, download.cleanupOutput(output))
		}
		return "", err
	}
	return output, nil
}

func (download *downloadTask) pause() error {
	download.mu.Lock()
	defer download.mu.Unlock()
	if download.task.State != model.TaskQueued && download.task.State != model.TaskRunning {
		return fmt.Errorf("download task %q is not pausable", download.task.ID)
	}
	download.task.State = model.TaskPaused
	download.generation++
	download.task.Pausable = false
	download.task.UpdatedAt = time.Now().UTC()
	if download.attempt != nil {
		download.attempt.cancel()
	}
	download.signalLocked()
	return nil
}

func (download *downloadTask) resume() error {
	download.mu.Lock()
	defer download.mu.Unlock()
	switch download.task.State {
	case model.TaskPaused:
		download.task.State = model.TaskQueued
		download.generation++
		download.task.Pausable = true
		download.task.UpdatedAt = time.Now().UTC()
	case model.TaskFailed:
		download.task.State = model.TaskQueued
		download.generation++
		download.task.Pausable = true
		download.task.Cancelable = true
		download.task.Retryable = false
		download.task.Error = download.cleanupErrorLocked()
		download.task.UpdatedAt = time.Now().UTC()
	default:
		return fmt.Errorf("download task %q is not resumable", download.task.ID)
	}
	download.signalLocked()
	return nil
}

func (download *downloadTask) retry() error {
	download.mu.Lock()
	defer download.mu.Unlock()
	if download.task.State != model.TaskFailed {
		return fmt.Errorf("download task %q is not retryable", download.task.ID)
	}
	download.task.State = model.TaskQueued
	download.generation++
	download.task.Pausable = true
	download.task.Cancelable = true
	download.task.Retryable = false
	download.task.Error = download.cleanupErrorLocked()
	download.task.UpdatedAt = time.Now().UTC()
	download.signalLocked()
	return nil
}

func (download *downloadTask) cancelTask() error {
	download.mu.Lock()
	defer download.mu.Unlock()
	if download.task.State == model.TaskCompleted {
		return fmt.Errorf("completed task cannot be canceled")
	}
	if download.task.State == model.TaskCanceled {
		return nil
	}
	download.task.State = model.TaskCanceled
	download.generation++
	download.task.Pausable = false
	download.task.Cancelable = false
	download.task.Retryable = false
	download.task.UpdatedAt = time.Now().UTC()
	if download.attempt != nil {
		download.attempt.cancel()
	}
	download.cancel()
	download.signalLocked()
	return nil
}

func (download *downloadTask) close() error {
	download.mu.Lock()
	completed := download.task.State == model.TaskCompleted
	download.mu.Unlock()
	if completed {
		return nil
	}
	err := download.cancelTask()
	<-download.done
	return err
}

func (download *downloadTask) finishAttempt(generation uint64, output string, attemptErr error) bool {
	download.mu.Lock()
	if generation != download.generation {
		download.mu.Unlock()
		if output != "" {
			download.cleanupStaleOutput(output)
		}
		return false
	}
	if download.ctx.Err() != nil || download.task.State == model.TaskCanceled {
		download.task.State = model.TaskCanceled
		download.task.Pausable = false
		download.task.Cancelable = false
		download.task.Retryable = false
		download.task.UpdatedAt = time.Now().UTC()
		download.mu.Unlock()
		if output != "" {
			download.cleanupStaleOutput(output)
		}
		return true
	}
	if download.task.State == model.TaskPaused {
		download.mu.Unlock()
		if output != "" {
			download.cleanupStaleOutput(output)
		}
		return false
	}
	if attemptErr != nil {
		download.task.State = model.TaskFailed
		download.task.Pausable = false
		download.task.Cancelable = true
		download.task.Retryable = true
		download.task.Error = attemptErr.Error()
		download.task.UpdatedAt = time.Now().UTC()
		download.signalLocked()
		download.mu.Unlock()
		return false
	}
	download.mu.Unlock()

	if err := download.directory.Verify(); err != nil {
		download.cleanupStaleOutput(output)
		download.mu.Lock()
		if generation == download.generation && download.task.State != model.TaskCanceled {
			download.task.State = model.TaskFailed
			download.task.Pausable = false
			download.task.Cancelable = true
			download.task.Retryable = true
			download.task.Error = errors.Join(err, download.cleanupErr).Error()
			download.task.UpdatedAt = time.Now().UTC()
			download.signalLocked()
		}
		download.mu.Unlock()
		return false
	}
	if hook := download.manager.beforeDownloadPublish; hook != nil {
		hook()
	}
	absolute := filepath.Join(download.root, output)
	download.manager.mu.Lock()
	download.mu.Lock()
	if generation != download.generation || download.manager.closed || download.ctx.Err() != nil ||
		download.task.State == model.TaskCanceled || download.task.State == model.TaskPaused {
		canceled := download.manager.closed || download.ctx.Err() != nil || download.task.State == model.TaskCanceled
		if canceled {
			download.task.State = model.TaskCanceled
			download.task.Pausable = false
			download.task.Cancelable = false
			download.task.Retryable = false
			download.task.UpdatedAt = time.Now().UTC()
		}
		download.mu.Unlock()
		download.manager.mu.Unlock()
		download.cleanupStaleOutput(output)
		return canceled
	}
	download.manager.downloadRecords[downloadPathKey(absolute)] = absolute
	download.task.State = model.TaskCompleted
	download.task.DownloadedBytes = download.task.TotalBytes
	download.task.Pausable = false
	download.task.Cancelable = false
	download.task.Retryable = false
	download.task.OutputPath = absolute
	download.task.Error = download.cleanupErrorLocked()
	download.task.UpdatedAt = time.Now().UTC()
	download.mu.Unlock()
	download.manager.mu.Unlock()
	return true
}

func (download *downloadTask) cleanupOutput(output string) error {
	if output == "" || output == "." || filepath.IsAbs(output) || !filepath.IsLocal(output) {
		return fmt.Errorf("invalid relative download output %q", output)
	}
	return download.manager.cleanupDownload(download.directory, output)
}

func (download *downloadTask) cleanupStaleOutput(output string) {
	if err := download.cleanupOutput(output); err != nil {
		download.mu.Lock()
		download.cleanupErr = errors.Join(download.cleanupErr, fmt.Errorf("clean stale download output: %w", err))
		download.task.Error = download.cleanupErrorLocked()
		download.task.UpdatedAt = time.Now().UTC()
		download.mu.Unlock()
	}
}

func (download *downloadTask) cleanupErrorLocked() string {
	if download.cleanupErr == nil {
		return ""
	}
	return download.cleanupErr.Error()
}

func (download *downloadTask) markCanceled() {
	download.mu.Lock()
	if download.task.State != model.TaskCompleted {
		download.task.State = model.TaskCanceled
		download.task.Pausable = false
		download.task.Cancelable = false
		download.task.Retryable = false
		download.task.UpdatedAt = time.Now().UTC()
	}
	download.mu.Unlock()
}

func (download *downloadTask) signalLocked() {
	select {
	case download.wake <- struct{}{}:
	default:
	}
}

type downloadProgressReader struct {
	download   *downloadTask
	reader     *os.File
	generation uint64
}

func (reader *downloadProgressReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	if read > 0 {
		reader.download.mu.Lock()
		if reader.generation == reader.download.generation {
			reader.download.task.DownloadedBytes += int64(read)
			reader.download.task.UpdatedAt = time.Now().UTC()
		}
		reader.download.mu.Unlock()
	}
	return read, err
}

func (manager *Manager) SetDownloadRoots(current string, managed []string) error {
	currentRoot, currentInfo, err := validatedDownloadRoot(current, true)
	if err != nil {
		return err
	}
	roots := []string{currentRoot}
	rootInfo := map[string]os.FileInfo{downloadPathKey(currentRoot): currentInfo}
	for _, candidate := range managed {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		root, info, err := validatedDownloadRoot(candidate, false)
		if err != nil {
			return fmt.Errorf("validate managed download root: %w", err)
		}
		duplicate := false
		for _, existing := range roots {
			if samePath(existing, root) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			roots = append(roots, root)
			rootInfo[downloadPathKey(root)] = info
		}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return fmt.Errorf("proxy manager is closed")
	}
	manager.downloadRoot = currentRoot
	manager.downloadRoots = roots
	manager.downloadRootInfo = rootInfo
	return nil
}

func (manager *Manager) SetDownloadRecords(paths []string) error {
	manager.mu.Lock()
	roots := append([]string(nil), manager.downloadRoots...)
	closed := manager.closed
	manager.mu.Unlock()
	if closed {
		return fmt.Errorf("proxy manager is closed")
	}
	records := make(map[string]string, len(paths))
	for _, path := range paths {
		normalized, err := normalizedDownloadPath(path)
		if err != nil {
			return err
		}
		if _, _, ok := downloadPathWithinRoots(normalized, roots); !ok {
			return fmt.Errorf("download record is outside the managed roots")
		}
		records[downloadPathKey(normalized)] = normalized
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return fmt.Errorf("proxy manager is closed")
	}
	manager.downloadRecords = records
	return nil
}

func (manager *Manager) DeleteDownload(path string) error {
	absolute, err := normalizedDownloadPath(path)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	roots := append([]string(nil), manager.downloadRoots...)
	recordedPath, recorded := manager.downloadRecords[downloadPathKey(absolute)]
	rootInfo := make(map[string]os.FileInfo, len(manager.downloadRootInfo))
	for key, info := range manager.downloadRootInfo {
		rootInfo[key] = info
	}
	closed := manager.closed
	manager.mu.Unlock()
	if closed {
		return fmt.Errorf("proxy manager is closed")
	}
	if !recorded || !samePath(recordedPath, absolute) {
		return fmt.Errorf("download path is not a recorded download")
	}
	rootPath, relative, ok := downloadPathWithinRoots(absolute, roots)
	if !ok {
		return fmt.Errorf("download path is outside the managed roots")
	}
	root, err := security.OpenDirectory(rootPath, false)
	if err != nil {
		return fmt.Errorf("open managed download root: %w", err)
	}
	defer root.Close()
	expectedInfo := rootInfo[downloadPathKey(rootPath)]
	if expectedInfo == nil || !os.SameFile(expectedInfo, root.Identity()) {
		return fmt.Errorf("managed download root identity changed")
	}
	if manager.beforeDownloadDelete != nil {
		manager.beforeDownloadDelete()
	}
	if err := root.RemoveRegular(relative); err != nil {
		return fmt.Errorf("delete recorded download: %w", err)
	}
	manager.mu.Lock()
	delete(manager.downloadRecords, downloadPathKey(absolute))
	manager.mu.Unlock()
	return nil
}

func validatedDownloadRoot(root string, create bool) (string, os.FileInfo, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil, fmt.Errorf("download root is required")
	}
	directory, err := security.OpenDirectory(root, create)
	if err != nil {
		return "", nil, fmt.Errorf("open download root: %w", err)
	}
	defer directory.Close()
	return directory.Path(), directory.Identity(), nil
}

func normalizedDownloadPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("download path is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve download path: %w", err)
	}
	return absolute, nil
}

func downloadPathWithinRoots(path string, roots []string) (string, string, bool) {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != "." && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return root, relative, true
		}
	}
	return "", "", false
}

func downloadPathKey(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
