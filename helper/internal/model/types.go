package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var kugouHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{16,128}$`)

func CacheKey(hash, quality, effect string) (string, error) {
	normalizedHash := strings.ToLower(strings.TrimSpace(hash))
	if !kugouHashPattern.MatchString(normalizedHash) {
		return "", fmt.Errorf("invalid Kugou hash")
	}
	if quality != "128" && quality != "320" && quality != "flac" && quality != "high" && quality != "super" {
		return "", fmt.Errorf("invalid quality")
	}
	if effect != "none" {
		return "", fmt.Errorf("unsupported effect")
	}
	return normalizedHash + "." + quality + "." + effect, nil
}

type Config struct {
	CacheRoot              string   `json:"cacheRoot"`
	DownloadRoot           string   `json:"downloadRoot"`
	ManagedDownloadRoots   []string `json:"managedDownloadRoots"`
	CompletedDownloadPaths []string `json:"completedDownloadPaths"`
	CacheLimitBytes        int64    `json:"cacheLimitBytes"`
}

type TaskState string
type TaskKind string

const (
	TaskQueued    TaskState = "queued"
	TaskBuffering TaskState = "buffering"
	TaskRunning   TaskState = "running"
	TaskPaused    TaskState = "paused"
	TaskNeedsURL  TaskState = "needs-url"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

const (
	TaskKindCache    TaskKind = "cache"
	TaskKindDownload TaskKind = "download"
)

type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type TrackInfo struct {
	Key              string  `json:"key"`
	CatalogHash      string  `json:"catalogHash"`
	Hash             string  `json:"hash"`
	RequestedQuality string  `json:"requestedQuality"`
	Quality          string  `json:"quality"`
	Effect           string  `json:"effect"`
	Title            string  `json:"title"`
	Artist           string  `json:"artist"`
	Album            string  `json:"album"`
	Extension        string  `json:"extension"`
	DurationSeconds  float64 `json:"durationSeconds"`
}

type CacheLookupRequest struct {
	Key              string `json:"key,omitempty"`
	CatalogHash      string `json:"catalogHash,omitempty"`
	RequestedQuality string `json:"requestedQuality,omitempty"`
	Effect           string `json:"effect,omitempty"`
}

type Task struct {
	ID              string    `json:"id"`
	Kind            TaskKind  `json:"kind"`
	Track           TrackInfo `json:"track"`
	State           TaskState `json:"state"`
	DownloadedBytes int64     `json:"downloadedBytes"`
	TotalBytes      int64     `json:"totalBytes"`
	BytesPerSecond  int64     `json:"bytesPerSecond"`
	Pausable        bool      `json:"pausable"`
	Cancelable      bool      `json:"cancelable"`
	Retryable       bool      `json:"retryable"`
	Error           string    `json:"error,omitempty"`
	OutputPath      string    `json:"outputPath,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type CacheLookup struct {
	Status string      `json:"status"`
	Key    string      `json:"key,omitempty"`
	Track  TrackInfo   `json:"track,omitempty"`
	Path   string      `json:"path,omitempty"`
	Size   int64       `json:"size,omitempty"`
	Ranges []ByteRange `json:"ranges,omitempty"`
}

type CreateSessionRequest struct {
	Track     TrackInfo `json:"track"`
	RemoteURL string    `json:"remoteUrl"`
}

type CreateSessionResponse struct {
	TaskID  string `json:"taskId"`
	PlayURL string `json:"playUrl"`
}
