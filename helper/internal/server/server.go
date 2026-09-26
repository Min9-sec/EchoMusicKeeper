package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/proxy"
)

const maxBodyBytes int64 = 1 << 20

const allowedMethods = "GET, PUT, POST, DELETE, OPTIONS"

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ProcessLauncher func(name string, args ...string) error

type Options struct {
	Token            string
	Manager          *proxy.Manager
	Config           model.Config
	Version          string
	DefaultMusicPath string
	Logger           *log.Logger
	Shutdown         func()
	LaunchProcess    ProcessLauncher
	HTTPClient       *http.Client
}

type handler struct {
	tokenHash        [sha256.Size]byte
	manager          *proxy.Manager
	configOps        sync.Mutex
	configMu         sync.RWMutex
	config           model.Config
	version          string
	defaultMusicPath string
	logger           *log.Logger
	shutdown         func()
	shutdownOnce     sync.Once
	launch           ProcessLauncher
	resolveCachePath func(string) (string, bool, error)
}

func New(options Options) http.Handler {
	launch := options.LaunchProcess
	if launch == nil {
		launch = launchFileManager
	}
	return &handler{
		tokenHash: sha256.Sum256([]byte(options.Token)), manager: options.Manager,
		config: options.Config, version: options.Version, defaultMusicPath: options.DefaultMusicPath,
		logger: options.Logger, shutdown: options.Shutdown, launch: launch,
		resolveCachePath: options.Manager.CompleteCachePath,
	}
}

func (server *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	stream := strings.HasPrefix(request.URL.Path, "/v1/stream/")
	if !stream {
		setControlHeaders(writer.Header())
	} else {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
	}
	if !loopbackRemote(request.RemoteAddr) {
		server.writeError(writer, http.StatusForbidden, "forbidden_remote", "request must originate from loopback")
		return
	}
	if !loopbackHost(request.Host) {
		server.writeError(writer, http.StatusForbidden, "forbidden_host", "Host must identify the loopback helper")
		return
	}
	if request.Method == http.MethodOptions && !stream {
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if stream {
		server.serveStream(writer, request)
		return
	}
	if !server.authorized(request.Header.Get("Authorization")) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="cache-helper"`)
		server.writeError(writer, http.StatusUnauthorized, "unauthorized", "valid bearer authorization is required")
		return
	}
	server.serveControl(writer, request)
}

func (server *handler) serveControl(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/v1/health":
		if !requireMethod(server, writer, request, http.MethodGet) {
			return
		}
		server.health(writer)
	case "/v1/config":
		if !requireMethod(server, writer, request, http.MethodPut) {
			return
		}
		server.updateConfig(writer, request)
	case "/v1/cache":
		if !requireMethod(server, writer, request, http.MethodGet) {
			return
		}
		server.cacheList(writer)
	case "/v1/cache/lookup":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.cacheLookup(writer, request)
	case "/v1/cache/clear":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.cacheClear(writer)
	case "/v1/cache/migrate":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.cacheMigrate(writer, request)
	case "/v1/proxy/sessions":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.createSession(writer, request)
	case "/v1/downloads":
		if request.Method == http.MethodPost {
			server.createDownload(writer, request)
		} else if request.Method == http.MethodDelete {
			server.deleteDownload(writer, request)
		} else {
			methodNotAllowed(server, writer, "POST, DELETE")
		}
	case "/v1/files/reveal":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.reveal(writer, request)
	case "/v1/tasks":
		if !requireMethod(server, writer, request, http.MethodGet) {
			return
		}
		server.writeJSON(writer, http.StatusOK, map[string]any{"tasks": server.manager.Tasks()})
	case "/v1/shutdown":
		if !requireMethod(server, writer, request, http.MethodPost) {
			return
		}
		server.beginShutdown(writer)
	default:
		if strings.HasPrefix(request.URL.Path, "/v1/tasks/") {
			server.taskAction(writer, request)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/v1/cache/") {
			server.deleteCache(writer, request)
			return
		}
		server.writeError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

func (server *handler) health(writer http.ResponseWriter) {
	view, err := server.manager.CacheView()
	if err != nil {
		server.internalError(writer, "health_unavailable", err)
		return
	}
	server.writeJSON(writer, http.StatusOK, map[string]any{
		"version": server.version, "platform": runtime.GOOS, "defaultMusicPath": server.defaultMusicPath,
		"cacheBytes": view.CacheBytes, "cacheLimitBytes": view.CacheLimitBytes,
	})
}

func (server *handler) updateConfig(writer http.ResponseWriter, request *http.Request) {
	server.configOps.Lock()
	defer server.configOps.Unlock()
	var next model.Config
	if !server.decodeJSON(writer, request, &next) {
		return
	}
	currentRoot, err := server.manager.CacheRoot()
	if err != nil {
		server.internalError(writer, "config_unavailable", err)
		return
	}
	if !samePath(next.CacheRoot, currentRoot) {
		server.writeError(writer, http.StatusConflict, "cache_migration_required", "change the cache root through /v1/cache/migrate first")
		return
	}
	if strings.TrimSpace(next.DownloadRoot) == "" || next.CacheLimitBytes < 0 {
		server.writeError(writer, http.StatusBadRequest, "invalid_config", "downloadRoot is required and cacheLimitBytes must not be negative")
		return
	}
	server.configMu.RLock()
	previous := cloneConfig(server.config)
	server.configMu.RUnlock()
	if err := server.manager.SetDownloadRoots(next.DownloadRoot, next.ManagedDownloadRoots); err != nil {
		server.writeError(writer, http.StatusBadRequest, "invalid_download_roots", "download roots could not be applied")
		return
	}
	if err := server.manager.SetDownloadRecords(next.CompletedDownloadPaths); err != nil {
		_ = server.manager.SetDownloadRoots(previous.DownloadRoot, previous.ManagedDownloadRoots)
		_ = server.manager.SetDownloadRecords(previous.CompletedDownloadPaths)
		server.writeError(writer, http.StatusBadRequest, "invalid_download_records", "completed download paths could not be restored")
		return
	}
	if err := server.manager.SetLimit(next.CacheLimitBytes); err != nil {
		_ = server.manager.SetDownloadRoots(previous.DownloadRoot, previous.ManagedDownloadRoots)
		_ = server.manager.SetDownloadRecords(previous.CompletedDownloadPaths)
		_ = server.manager.SetLimit(previous.CacheLimitBytes)
		server.internalError(writer, "cache_limit_failed", err)
		return
	}
	next.CacheRoot = currentRoot
	server.configMu.Lock()
	server.config = cloneConfig(next)
	server.configMu.Unlock()
	server.writeJSON(writer, http.StatusOK, next)
}

func (server *handler) cacheList(writer http.ResponseWriter) {
	view, err := server.manager.CacheView()
	if err != nil {
		server.internalError(writer, "cache_unavailable", err)
		return
	}
	server.writeJSON(writer, http.StatusOK, view)
}

func (server *handler) cacheLookup(writer http.ResponseWriter, request *http.Request) {
	var body model.CacheLookupRequest
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	lookup, err := server.manager.Lookup(body)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "invalid_cache_lookup", "cache lookup request is invalid")
		return
	}
	if lookup.Status == "complete" {
		path, found, err := server.resolveCachePath(lookup.Key)
		if err != nil {
			server.internalError(writer, "cache_path_unavailable", err)
			return
		}
		if !found {
			lookup = model.CacheLookup{Status: "miss"}
		} else {
			lookup.Path = path
		}
	}
	server.writeJSON(writer, http.StatusOK, lookup)
}

func (server *handler) cacheClear(writer http.ResponseWriter) {
	result, err := server.manager.ClearCache()
	if err != nil {
		server.internalError(writer, "cache_clear_failed", err)
		return
	}
	server.writeJSON(writer, http.StatusOK, result)
}

func (server *handler) deleteCache(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(server, writer, request, http.MethodDelete) {
		return
	}
	rawKey := strings.TrimPrefix(request.URL.Path, "/v1/cache/")
	key, err := url.PathUnescape(rawKey)
	if err != nil || key == "" || strings.Contains(key, "/") {
		server.writeError(writer, http.StatusBadRequest, "invalid_cache_key", "cache key is invalid")
		return
	}
	deleted, err := server.manager.DeleteCache(key)
	if errors.Is(err, proxy.ErrCacheEntryActive) {
		server.writeError(writer, http.StatusConflict, "cache_entry_active", "active cache entries cannot be deleted")
		return
	}
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "cache_delete_failed", "cache entry could not be deleted")
		return
	}
	server.writeJSON(writer, http.StatusOK, map[string]bool{"deleted": deleted})
}

type migrationRequest struct {
	CacheRoot string `json:"cacheRoot"`
	Mode      string `json:"mode"`
	DeleteOld bool   `json:"deleteOld"`
}

func (server *handler) cacheMigrate(writer http.ResponseWriter, request *http.Request) {
	server.configOps.Lock()
	defer server.configOps.Unlock()
	var body migrationRequest
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	var err error
	switch body.Mode {
	case "migrate":
		err = server.manager.MigrateCache(body.CacheRoot, body.DeleteOld)
	case "fresh":
		if body.DeleteOld {
			server.writeError(writer, http.StatusBadRequest, "invalid_migration", "fresh mode cannot delete the previous cache root")
			return
		}
		err = server.manager.SwitchCacheRoot(body.CacheRoot)
	default:
		server.writeError(writer, http.StatusBadRequest, "invalid_migration", "mode must be migrate or fresh")
		return
	}
	if err != nil {
		server.writeError(writer, http.StatusConflict, "cache_migration_failed", "cache root could not be switched")
		return
	}
	root, err := server.manager.CacheRoot()
	if err != nil {
		server.internalError(writer, "cache_unavailable", err)
		return
	}
	server.configMu.Lock()
	server.config.CacheRoot = root
	server.configMu.Unlock()
	server.writeJSON(writer, http.StatusOK, map[string]string{"cacheRoot": root, "mode": body.Mode})
}

func (server *handler) createSession(writer http.ResponseWriter, request *http.Request) {
	var body model.CreateSessionRequest
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	session, err := server.manager.CreateOrReuse(request.Context(), body)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "session_failed", "playback session could not be created")
		return
	}
	playURL := "http://" + request.Host + "/v1/stream/" + url.PathEscape(session.ID()) + "?token=" + url.QueryEscape(session.PlaybackToken())
	server.writeJSON(writer, http.StatusOK, model.CreateSessionResponse{TaskID: session.ID(), PlayURL: playURL})
}

type downloadRequest struct {
	Key       string          `json:"key,omitempty"`
	Track     model.TrackInfo `json:"track,omitempty"`
	RemoteURL string          `json:"remoteUrl,omitempty"`
}

func (server *handler) createDownload(writer http.ResponseWriter, request *http.Request) {
	server.configOps.Lock()
	defer server.configOps.Unlock()
	var body downloadRequest
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	var task model.Task
	var err error
	if body.Key != "" {
		server.configMu.RLock()
		root := server.config.DownloadRoot
		server.configMu.RUnlock()
		task, err = server.manager.ExportWhenComplete(body.Key, root)
	} else {
		task, err = server.manager.CreateDownload(request.Context(), model.CreateSessionRequest{Track: body.Track, RemoteURL: body.RemoteURL})
	}
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "download_failed", "download task could not be created")
		return
	}
	server.writeJSON(writer, http.StatusAccepted, task)
}

func (server *handler) deleteDownload(writer http.ResponseWriter, request *http.Request) {
	server.configOps.Lock()
	defer server.configOps.Unlock()
	var body struct {
		Path string `json:"path"`
	}
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	if err := server.manager.DeleteDownload(body.Path); err != nil {
		server.writeError(writer, http.StatusBadRequest, "download_delete_failed", "recorded download could not be deleted")
		return
	}
	server.configMu.Lock()
	remaining := server.config.CompletedDownloadPaths[:0]
	for _, path := range server.config.CompletedDownloadPaths {
		if !samePath(path, body.Path) {
			remaining = append(remaining, path)
		}
	}
	server.config.CompletedDownloadPaths = append([]string(nil), remaining...)
	server.configMu.Unlock()
	server.writeJSON(writer, http.StatusOK, map[string]bool{"deleted": true})
}

func (server *handler) taskAction(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/v1/tasks/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		server.writeError(writer, http.StatusNotFound, "not_found", "task route not found")
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || strings.Contains(id, "/") {
		server.writeError(writer, http.StatusBadRequest, "invalid_task_id", "task id is invalid")
		return
	}
	action := parts[1]
	if action == "url" {
		if !requireMethod(server, writer, request, http.MethodPut) {
			return
		}
		var body struct {
			RemoteURL string `json:"remoteUrl"`
		}
		if !server.decodeJSON(writer, request, &body) {
			return
		}
		key := ""
		for _, task := range server.manager.Tasks() {
			if task.ID == id {
				key = task.Track.Key
				break
			}
		}
		if key == "" || server.manager.UpdateURL(key, body.RemoteURL) != nil {
			server.writeError(writer, http.StatusBadRequest, "task_url_failed", "task URL could not be updated")
			return
		}
		server.writeJSON(writer, http.StatusOK, map[string]bool{"updated": true})
		return
	}
	if !requireMethod(server, writer, request, http.MethodPost) {
		return
	}
	switch action {
	case "pause":
		err = server.manager.Pause(id)
	case "resume":
		err = server.manager.Resume(id)
	case "cancel":
		err = server.manager.Cancel(id)
	default:
		server.writeError(writer, http.StatusNotFound, "not_found", "task action not found")
		return
	}
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "task_action_failed", "task action could not be completed")
		return
	}
	server.writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (server *handler) reveal(writer http.ResponseWriter, request *http.Request) {
	server.configOps.Lock()
	defer server.configOps.Unlock()
	var body struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	if !server.decodeJSON(writer, request, &body) {
		return
	}
	target, err := server.manager.RevealTarget(body.Kind, body.Path)
	if err != nil {
		server.writeError(writer, http.StatusBadRequest, "reveal_forbidden", "path is not an exact managed target")
		return
	}
	executable, args, err := revealCommand(body.Kind, target)
	if err != nil {
		server.internalError(writer, "reveal_failed", err)
		return
	}
	if err := server.launch(executable, args...); err != nil {
		server.internalError(writer, "reveal_failed", err)
		return
	}
	server.writeJSON(writer, http.StatusAccepted, map[string]bool{"launched": true})
}

func (server *handler) serveStream(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		server.writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	id, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/v1/stream/"))
	if err != nil || id == "" || strings.Contains(id, "/") {
		server.writeError(writer, http.StatusNotFound, "stream_not_found", "playback session was not found")
		return
	}
	session, ok := server.manager.Session(id, request.URL.Query().Get("token"))
	if !ok {
		server.writeError(writer, http.StatusUnauthorized, "invalid_playback_token", "valid playback authorization is required")
		return
	}
	session.ServeHTTP(writer, request)
}

func (server *handler) beginShutdown(writer http.ResponseWriter) {
	server.writeJSON(writer, http.StatusAccepted, map[string]string{"status": "shutting-down"})
	server.shutdownOnce.Do(func() {
		if server.shutdown != nil {
			server.shutdown()
		}
	})
}

func (server *handler) authorized(value string) bool {
	presented := ""
	if strings.HasPrefix(value, "Bearer ") && !strings.ContainsAny(strings.TrimPrefix(value, "Bearer "), " \t\r\n") {
		presented = strings.TrimPrefix(value, "Bearer ")
	}
	presentedHash := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(server.tokenHash[:], presentedHash[:]) == 1 && presented != ""
}

func (server *handler) decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	if request.ContentLength > maxBodyBytes {
		server.writeError(writer, http.StatusRequestEntityTooLarge, "body_too_large", "JSON body exceeds 1 MiB")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			server.writeError(writer, http.StatusRequestEntityTooLarge, "body_too_large", "JSON body exceeds 1 MiB")
		} else {
			server.writeError(writer, http.StatusBadRequest, "invalid_json", "request body must contain one valid JSON value")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		server.writeError(writer, http.StatusBadRequest, "invalid_json", "request body must contain one valid JSON value")
		return false
	}
	return true
}

func (server *handler) internalError(writer http.ResponseWriter, code string, err error) {
	if server.logger != nil {
		server.logger.Printf("%s: %v", code, err)
	}
	server.writeError(writer, http.StatusInternalServerError, code, "helper operation failed")
}

func (server *handler) writeError(writer http.ResponseWriter, status int, code, message string) {
	server.writeJSON(writer, status, ErrorResponse{Code: code, Message: message})
}

func (server *handler) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func requireMethod(server *handler, writer http.ResponseWriter, request *http.Request, method string) bool {
	if request.Method == method {
		return true
	}
	methodNotAllowed(server, writer, method)
	return false
}

func methodNotAllowed(server *handler, writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	server.writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}

func setControlHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Methods", allowedMethods)
	header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	header.Set("Access-Control-Allow-Private-Network", "true")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackHost(value string) bool {
	host := value
	if parsed, _, err := net.SplitHostPort(value); err == nil {
		host = parsed
	} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	} else if strings.Contains(value, ":") {
		return false
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

func cloneConfig(config model.Config) model.Config {
	config.ManagedDownloadRoots = append([]string(nil), config.ManagedDownloadRoots...)
	config.CompletedDownloadPaths = append([]string(nil), config.CompletedDownloadPaths...)
	return config
}

func samePath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// revealCommand returns the platform file manager invocation that selects or
// opens one already authorized target. Only platforms with a supported file
// manager receive a command.
func revealCommand(kind, target string) (string, []string, error) {
	return revealCommandFor(runtime.GOOS, kind, target)
}

func revealCommandFor(goos, kind, target string) (string, []string, error) {
	switch goos {
	case "windows":
		if kind == "select" {
			return "explorer.exe", []string{"/select," + target}, nil
		}
		return "explorer.exe", []string{target}, nil
	case "darwin":
		if kind == "select" {
			return "open", []string{"-R", target}, nil
		}
		return "open", []string{target}, nil
	default:
		return "", nil, fmt.Errorf("file manager reveal is only available on Windows and macOS")
	}
}

func launchFileManager(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}
