package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/proxy"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/testserver"
)

const (
	testBearer = "test-token"
	testHash   = "0123456789abcdef"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestEveryControlRouteRequiresBearerAuthentication(t *testing.T) {
	handler, cleanup := newTestHandler(t, Options{})
	defer cleanup()
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/health"},
		{http.MethodPut, "/v1/config"},
		{http.MethodGet, "/v1/cache"},
		{http.MethodPost, "/v1/cache/lookup"},
		{http.MethodPost, "/v1/proxy/sessions"},
		{http.MethodPost, "/v1/downloads"},
		{http.MethodDelete, "/v1/downloads"},
		{http.MethodPost, "/v1/files/reveal"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodPost, "/v1/tasks/task-id/pause"},
		{http.MethodPost, "/v1/tasks/task-id/resume"},
		{http.MethodPost, "/v1/tasks/task-id/cancel"},
		{http.MethodPut, "/v1/tasks/task-id/url"},
		{http.MethodPost, "/v1/cache/clear"},
		{http.MethodPost, "/v1/cache/migrate"},
		{http.MethodDelete, "/v1/cache/" + testHash + ".320.none"},
		{http.MethodPost, "/v1/shutdown"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			for _, authorization := range []string{"", "Bearer wrong-token", "Basic dGVzdA=="} {
				request := httptest.NewRequest(route.method, "http://127.0.0.1"+route.path, strings.NewReader("{}"))
				request.RemoteAddr = "127.0.0.1:32123"
				request.Header.Set("Authorization", authorization)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("authorization %q: status = %d, want 401", authorization, response.Code)
				}
				assertJSONErrorHeaders(t, response)
			}
		})
	}
}

func TestLoopbackHostAndCORSChecksPrecedePreflight(t *testing.T) {
	handler, cleanup := newTestHandler(t, Options{})
	defer cleanup()

	nonLoopback := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/v1/health", nil)
	nonLoopback.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, nonLoopback)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status = %d, want 403", response.Code)
	}

	badHost := httptest.NewRequest(http.MethodOptions, "http://attacker.example/v1/health", nil)
	badHost.RemoteAddr = "127.0.0.1:1234"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, badHost)
	if response.Code != http.StatusForbidden {
		t.Fatalf("bad Host status = %d, want 403", response.Code)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "http://localhost/v1/health", nil)
	preflight.RemoteAddr = "[::1]:1234"
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPut)
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Methods"); strings.Contains(got, http.MethodConnect) || !strings.Contains(got, http.MethodDelete) {
		t.Fatalf("allow methods = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Fatalf("allow headers = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("private-network header = %q", got)
	}
}

func TestHealthConfigBodyLimitAndStructuredErrors(t *testing.T) {
	downloadRoot := t.TempDir()
	recorded := filepath.Join(downloadRoot, "restored.mp3")
	if err := os.WriteFile(recorded, []byte("download"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := model.Config{
		CacheRoot: filepath.Join(t.TempDir(), "cache"), DownloadRoot: downloadRoot,
		ManagedDownloadRoots: []string{downloadRoot}, CompletedDownloadPaths: []string{recorded}, CacheLimitBytes: 4096,
	}
	handler, cleanup := newTestHandler(t, Options{Config: config, DefaultMusicPath: downloadRoot})
	defer cleanup()

	response := serveAuthorized(handler, http.MethodGet, "/v1/health", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", response.Code, response.Body.String())
	}
	var health struct {
		Version          string `json:"version"`
		Platform         string `json:"platform"`
		DefaultMusicPath string `json:"defaultMusicPath"`
		CacheBytes       int64  `json:"cacheBytes"`
		CacheLimitBytes  int64  `json:"cacheLimitBytes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Version != "test" || health.Platform == "" || health.DefaultMusicPath != downloadRoot || health.CacheBytes != 0 || health.CacheLimitBytes != 4096 {
		t.Fatalf("health = %+v", health)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %v", response.Header())
	}

	oversized := bytes.NewReader(bytes.Repeat([]byte("x"), int(maxBodyBytes+1)))
	response = serveAuthorized(handler, http.MethodPut, "/v1/config", oversized)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413: %s", response.Code, response.Body.String())
	}
	assertJSONErrorHeaders(t, response)

	changed := config
	changed.CacheRoot = filepath.Join(t.TempDir(), "different-cache")
	response = serveJSONAuthorized(handler, http.MethodPut, "/v1/config", changed)
	if response.Code != http.StatusConflict {
		t.Fatalf("changed cache root status = %d, want 409: %s", response.Code, response.Body.String())
	}

	restored := filepath.Join(downloadRoot, "restored-after-put.mp3")
	if err := os.WriteFile(restored, []byte("restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.CompletedDownloadPaths = []string{restored}
	response = serveJSONAuthorized(handler, http.MethodPut, "/v1/config", config)
	if response.Code != http.StatusOK {
		t.Fatalf("restore config status = %d: %s", response.Code, response.Body.String())
	}
	response = serveJSONAuthorized(handler, http.MethodDelete, "/v1/downloads", map[string]string{"path": restored})
	if response.Code != http.StatusOK {
		t.Fatalf("restored download delete status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Fatalf("restored download remains: %v", err)
	}
}

func TestRevealUsesExactAuthorizedTargetAndSeparateExplorerArgument(t *testing.T) {
	downloadRoot := t.TempDir()
	recorded := filepath.Join(downloadRoot, "recorded.mp3")
	arbitrary := filepath.Join(downloadRoot, "arbitrary.mp3")
	for _, path := range []string{recorded, arbitrary} {
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var executable string
	var arguments []string
	handler, cleanup := newTestHandler(t, Options{
		Config: model.Config{DownloadRoot: downloadRoot, CompletedDownloadPaths: []string{recorded}},
		LaunchProcess: func(name string, args ...string) error {
			executable = name
			arguments = append([]string(nil), args...)
			return nil
		},
	})
	defer cleanup()
	response := serveJSONAuthorized(handler, http.MethodPost, "/v1/files/reveal", map[string]string{"kind": "select", "path": recorded})
	if response.Code != http.StatusAccepted || executable != "explorer.exe" || len(arguments) != 1 || arguments[0] != "/select,"+recorded {
		t.Fatalf("select reveal = %d, %q, %#v: %s", response.Code, executable, arguments, response.Body.String())
	}
	response = serveJSONAuthorized(handler, http.MethodPost, "/v1/files/reveal", map[string]string{"kind": "open-directory", "path": downloadRoot})
	if response.Code != http.StatusAccepted || len(arguments) != 1 || arguments[0] != downloadRoot {
		t.Fatalf("directory reveal = %d, %#v: %s", response.Code, arguments, response.Body.String())
	}
	response = serveJSONAuthorized(handler, http.MethodPost, "/v1/files/reveal", map[string]string{"kind": "select", "path": arbitrary})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary reveal status = %d, want 400", response.Code)
	}
}

func TestProxySessionReuseAndPlaybackTokenRanges(t *testing.T) {
	media := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	client := rangeTestClient(t, media)
	handler, cleanup := newTestHandler(t, Options{HTTPClient: client})
	defer cleanup()
	requestBody := model.CreateSessionRequest{
		Track:     model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"},
		RemoteURL: "http://8.8.8.8/audio?signature=secret",
	}
	first := serveJSONAuthorized(handler, http.MethodPost, "/v1/proxy/sessions", requestBody)
	second := serveJSONAuthorized(handler, http.MethodPost, "/v1/proxy/sessions", requestBody)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("session statuses = %d/%d: %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstSession, secondSession model.CreateSessionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstSession); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondSession); err != nil {
		t.Fatal(err)
	}
	if firstSession.PlayURL == "" || firstSession.PlayURL != secondSession.PlayURL || firstSession.TaskID != secondSession.TaskID {
		t.Fatalf("sessions = %+v / %+v", firstSession, secondSession)
	}
	parsed, err := url.Parse(firstSession.PlayURL)
	if err != nil {
		t.Fatal(err)
	}
	withoutToken := *parsed
	withoutToken.RawQuery = ""
	response := serveRequest(handler, http.MethodGet, withoutToken.RequestURI(), nil, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing playback token status = %d, want 401", response.Code)
	}
	for _, requestedRange := range []string{"bytes=0-4", "bytes=5-9"} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+parsed.RequestURI(), nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("Range", requestedRange)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusPartialContent || response.Header().Get("Accept-Ranges") != "bytes" || response.Header().Get("Content-Range") == "" {
			t.Fatalf("range %q response = %d %v: %s", requestedRange, response.Code, response.Header(), response.Body.String())
		}
	}
}

func TestExactRouteMethodsRejectRetryHeadAndUnknownRoutes(t *testing.T) {
	handler, cleanup := newTestHandler(t, Options{})
	defer cleanup()
	response := serveAuthorized(handler, http.MethodPost, "/v1/tasks/task-id/retry", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("retry route status = %d, want 404: %s", response.Code, response.Body.String())
	}
	response = serveAuthorized(handler, http.MethodGet, "/v1/proxy/sessions", nil)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("wrong method response = %d %v", response.Code, response.Header())
	}
	response = serveAuthorized(handler, http.MethodGet, "/v1/unknown", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", response.Code)
	}

	created := serveJSONAuthorized(handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{
		Track:     model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"},
		RemoteURL: "http://8.8.8.8/head",
	})
	var session model.CreateSessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(session.PlayURL)
	if err != nil {
		t.Fatal(err)
	}
	response = serveRequest(handler, http.MethodHead, parsed.RequestURI(), nil, "")
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD stream response = %d %v, want 405 Allow GET", response.Code, response.Header())
	}
}

func TestShutdownOnlySignalsEvenWhenProducerCloseIsStuck(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	client := handlerTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}))
	shutdown := make(chan struct{}, 1)
	handler, cleanup := newTestHandler(t, Options{
		HTTPClient: client,
		Shutdown: func() {
			select {
			case shutdown <- struct{}{}:
			default:
			}
		},
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		cleanup()
	}()
	created := serveJSONAuthorized(handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{
		Track:     model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"},
		RemoteURL: "http://8.8.8.8/stuck",
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("producer did not start")
	}
	begin := time.Now()
	response := serveAuthorized(handler, http.MethodPost, "/v1/shutdown", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("shutdown status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("shutdown callback waited for stuck manager close")
	}
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("shutdown signal elapsed = %s", elapsed)
	}
	releaseOnce.Do(func() { close(release) })
}

func TestShutdownSignalsAndLogsOmitSecrets(t *testing.T) {
	var logs bytes.Buffer
	shutdown := make(chan struct{}, 1)
	handler, cleanup := newTestHandler(t, Options{
		Logger: log.New(&logs, "", 0),
		Shutdown: func() {
			select {
			case shutdown <- struct{}{}:
			default:
			}
		},
	})
	defer cleanup()

	response := serveJSONAuthorized(handler, http.MethodPost, "/v1/proxy/sessions", map[string]any{
		"track":     map[string]any{"hash": testHash, "catalogHash": testHash, "requestedQuality": "320", "quality": "320", "effect": "none", "extension": "mp3"},
		"remoteUrl": "http://8.8.8.8/audio?account=private",
	})
	var session model.CreateSessionResponse
	_ = json.Unmarshal(response.Body.Bytes(), &session)
	response = serveAuthorized(handler, http.MethodPost, "/v1/shutdown", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("shutdown status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not signaled")
	}
	for _, secret := range []string{testBearer, "account=private", session.PlayURL} {
		if secret != "" && strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logs.String())
		}
	}
}

func TestCacheRoutesWireListLookupDeleteClearAndMigration(t *testing.T) {
	fixture := newTestFixture(t, Options{})
	defer fixture.cleanup()
	first := addCompletedServerEntry(t, fixture.index, testHash, []byte("first cache"))
	second := addCompletedServerEntry(t, fixture.index, "fedcba9876543210", []byte("second cache"))

	response := serveAuthorized(fixture.handler, http.MethodGet, "/v1/cache", nil)
	var view proxy.CacheView
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || len(view.Items) != 2 {
		t.Fatalf("cache list = %d %+v: %s", response.Code, view, response.Body.String())
	}
	response = serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", model.CacheLookupRequest{Key: first.Key})
	var lookup model.CacheLookup
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &lookup) != nil || lookup.Status != "complete" {
		t.Fatalf("cache lookup = %d %+v: %s", response.Code, lookup, response.Body.String())
	}
	if !filepath.IsAbs(lookup.Path) {
		t.Fatalf("cache lookup path = %q, want absolute", lookup.Path)
	}
	if contents, err := os.ReadFile(lookup.Path); err != nil || string(contents) != "first cache" {
		t.Fatalf("cache lookup contents = %q, %v", contents, err)
	}
	response = serveAuthorized(fixture.handler, http.MethodDelete, "/v1/cache/"+url.PathEscape(first.Key), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("cache delete = %d: %s", response.Code, response.Body.String())
	}
	if lookup, err := fixture.manager.Lookup(model.CacheLookupRequest{Key: first.Key}); err != nil || lookup.Status != "miss" {
		t.Fatalf("deleted lookup = %+v, %v", lookup, err)
	}
	response = serveAuthorized(fixture.handler, http.MethodPost, "/v1/cache/clear", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("cache clear = %d: %s", response.Code, response.Body.String())
	}
	if lookup, err := fixture.manager.Lookup(model.CacheLookupRequest{Key: second.Key}); err != nil || lookup.Status != "miss" {
		t.Fatalf("cleared lookup = %+v, %v", lookup, err)
	}
	newRoot := filepath.Join(t.TempDir(), "fresh-cache")
	response = serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/migrate", map[string]any{"cacheRoot": newRoot, "mode": "fresh"})
	if response.Code != http.StatusOK {
		t.Fatalf("cache migration = %d: %s", response.Code, response.Body.String())
	}
	if root, err := fixture.manager.CacheRoot(); err != nil || !samePath(root, newRoot) {
		t.Fatalf("selected cache root = %q, %v", root, err)
	}
}

func TestCacheLookupUsesCustomAndMigratedAbsoluteRootsAndValidatesFiles(t *testing.T) {
	customRoot := filepath.Join(t.TempDir(), "custom-cache")
	fixture := newTestFixture(t, Options{Config: model.Config{CacheRoot: customRoot}})
	defer fixture.cleanup()
	track := addCompletedServerEntry(t, fixture.index, testHash, []byte("custom root cache"))

	lookupComplete := func() model.CacheLookup {
		response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", model.CacheLookupRequest{Key: track.Key})
		var lookup model.CacheLookup
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &lookup) != nil {
			t.Fatalf("cache lookup = %d: %s", response.Code, response.Body.String())
		}
		return lookup
	}
	lookup := lookupComplete()
	want := filepath.Join(customRoot, "audio", track.Key+".mp3")
	if lookup.Status != "complete" || !filepath.IsAbs(lookup.Path) || !samePath(lookup.Path, want) {
		t.Fatalf("custom lookup = %+v, want path %q", lookup, want)
	}

	migratedRoot := filepath.Join(t.TempDir(), "migrated-cache")
	response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/migrate", map[string]any{"cacheRoot": migratedRoot, "mode": "migrate"})
	if response.Code != http.StatusOK {
		t.Fatalf("cache migration = %d: %s", response.Code, response.Body.String())
	}
	lookup = lookupComplete()
	want = filepath.Join(migratedRoot, "audio", track.Key+".mp3")
	if lookup.Status != "complete" || !samePath(lookup.Path, want) {
		t.Fatalf("migrated lookup = %+v, want path %q", lookup, want)
	}
	if contents, err := os.ReadFile(lookup.Path); err != nil || string(contents) != "custom root cache" {
		t.Fatalf("migrated lookup contents = %q, %v", contents, err)
	}
	if err := os.Remove(lookup.Path); err != nil {
		t.Fatal(err)
	}
	lookup = lookupComplete()
	if lookup.Status != "miss" || lookup.Path != "" {
		t.Fatalf("stale lookup = %+v, want pathless miss", lookup)
	}
}

func TestCacheLookupLeavesPartialAndMissPathsEmpty(t *testing.T) {
	fixture := newTestFixture(t, Options{})
	defer fixture.cleanup()
	partial := addPartialServerEntry(t, fixture.index, testHash, []byte("partial"))
	for name, request := range map[string]model.CacheLookupRequest{
		"partial": {Key: partial.Key},
		"miss":    {Key: "fedcba9876543210.320.none"},
	} {
		response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", request)
		var lookup model.CacheLookup
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &lookup) != nil {
			t.Fatalf("%s lookup = %d: %s", name, response.Code, response.Body.String())
		}
		if lookup.Status != name || lookup.Path != "" {
			t.Fatalf("%s lookup = %+v", name, lookup)
		}
	}
}

func TestCacheLookupTouchesExactAndAliasCompleteHits(t *testing.T) {
	fixture := newTestFixture(t, Options{})
	defer fixture.cleanup()
	track := addCompletedServerEntry(t, fixture.index, testHash, []byte("touch through server"))
	requests := []struct {
		name    string
		request model.CacheLookupRequest
	}{
		{name: "exact", request: model.CacheLookupRequest{Key: track.Key}},
		{name: "alias", request: model.CacheLookupRequest{
			CatalogHash: track.CatalogHash, RequestedQuality: track.RequestedQuality, Effect: track.Effect,
		}},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			entry := fixture.index.Snapshot()[track.Key]
			before := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			entry.LastAccessedAt = before
			if err := fixture.index.Upsert(track.Key, entry); err != nil {
				t.Fatal(err)
			}
			response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", test.request)
			var lookup model.CacheLookup
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &lookup) != nil {
				t.Fatalf("lookup = %d: %s", response.Code, response.Body.String())
			}
			if lookup.Status != "complete" || lookup.Key != track.Key || !filepath.IsAbs(lookup.Path) {
				t.Fatalf("lookup = %+v", lookup)
			}
			if contents, err := os.ReadFile(lookup.Path); err != nil || string(contents) != "touch through server" {
				t.Fatalf("lookup contents = %q, %v", contents, err)
			}
			if got := fixture.index.Snapshot()[track.Key].LastAccessedAt; !got.After(before) {
				t.Fatalf("last accessed = %v, want after %v", got, before)
			}
		})
	}
}

func TestCacheLookupDoesNotTouchPartialOrMiss(t *testing.T) {
	fixture := newTestFixture(t, Options{})
	defer fixture.cleanup()
	partial := addPartialServerEntry(t, fixture.index, testHash, []byte("partial untouched"))
	before := fixture.index.Snapshot()[partial.Key].LastAccessedAt

	for name, request := range map[string]model.CacheLookupRequest{
		"partial": {Key: partial.Key},
		"miss":    {Key: "fedcba9876543210.320.none"},
	} {
		response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s lookup = %d: %s", name, response.Code, response.Body.String())
		}
	}
	if got := fixture.index.Snapshot()[partial.Key].LastAccessedAt; !got.Equal(before) {
		t.Fatalf("partial last accessed = %v, want %v", got, before)
	}
}

func TestCacheLookupPostLookupStaleFilesReturnCleanMiss(t *testing.T) {
	for _, test := range []struct {
		name    string
		request func(model.TrackInfo) model.CacheLookupRequest
		mutate  func(string) error
	}{
		{name: "missing exact", request: func(track model.TrackInfo) model.CacheLookupRequest {
			return model.CacheLookupRequest{Key: track.Key}
		}, mutate: os.Remove},
		{name: "nonregular alias", request: func(track model.TrackInfo) model.CacheLookupRequest {
			return model.CacheLookupRequest{CatalogHash: track.CatalogHash, RequestedQuality: track.RequestedQuality, Effect: track.Effect}
		}, mutate: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestFixture(t, Options{})
			defer fixture.cleanup()
			track := addCompletedServerEntry(t, fixture.index, testHash, []byte("stale through server"))
			path := filepath.Join(fixture.index.Root(), "audio", track.Key+".mp3")
			server := fixture.handler.(*handler)
			resolve := server.resolveCachePath
			server.resolveCachePath = func(key string) (string, bool, error) {
				if err := test.mutate(path); err != nil {
					t.Fatal(err)
				}
				return resolve(key)
			}

			response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", test.request(track))
			var lookup model.CacheLookup
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &lookup) != nil {
				t.Fatalf("lookup = %d: %s", response.Code, response.Body.String())
			}
			if lookup.Status != "miss" || lookup.Key != "" || lookup.Track != (model.TrackInfo{}) ||
				lookup.Path != "" || lookup.Size != 0 || len(lookup.Ranges) != 0 {
				t.Fatalf("stale lookup = %+v, want clean miss", lookup)
			}
		})
	}
}

func TestCacheLookupCompletePathFailureReturnsStructuredServerError(t *testing.T) {
	fixture := newTestFixture(t, Options{})
	defer fixture.cleanup()
	track := addCompletedServerEntry(t, fixture.index, testHash, []byte("failed touch"))
	server := fixture.handler.(*handler)
	server.resolveCachePath = func(string) (string, bool, error) {
		return "", false, errors.New("persist touch failed")
	}

	response := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/cache/lookup", model.CacheLookupRequest{Key: track.Key})
	var body ErrorResponse
	if response.Code != http.StatusInternalServerError || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("lookup failure = %d: %s", response.Code, response.Body.String())
	}
	if body.Code != "cache_path_unavailable" {
		t.Fatalf("lookup failure = %+v", body)
	}
}

func TestDownloadAndTaskActionRoutesWirePauseResumeCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	client := handlerTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}))
	fixture := newTestFixture(t, Options{HTTPClient: client})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		fixture.cleanup()
	}()
	track := model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"}
	created := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{Track: track, RemoteURL: "http://8.8.8.8/task"})
	var session model.CreateSessionResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("session create = %d: %s", created.Code, created.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache producer did not start")
	}
	key, _ := model.CacheKey(testHash, "320", "none")
	downloadResponse := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/downloads", map[string]string{"key": key})
	var download model.Task
	if downloadResponse.Code != http.StatusAccepted || json.Unmarshal(downloadResponse.Body.Bytes(), &download) != nil || download.ID == "" {
		t.Fatalf("download create = %d: %s", downloadResponse.Code, downloadResponse.Body.String())
	}
	for _, action := range []string{"pause", "resume", "cancel"} {
		response := serveAuthorized(fixture.handler, http.MethodPost, "/v1/tasks/"+download.ID+"/"+action, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("task %s = %d: %s", action, response.Code, response.Body.String())
		}
	}
	response := serveAuthorized(fixture.handler, http.MethodGet, "/v1/tasks", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), download.ID) {
		t.Fatalf("task list = %d: %s", response.Code, response.Body.String())
	}
	releaseOnce.Do(func() { close(release) })
}

func TestTaskURLRefreshRouteWiresNeedsURLSession(t *testing.T) {
	var requests atomic.Int32
	media := []byte("refreshed media")
	client := handlerTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(writer, "expired", http.StatusForbidden)
			return
		}
		serveRangeMedia(writer, request, media)
	}))
	fixture := newTestFixture(t, Options{HTTPClient: client})
	defer fixture.cleanup()
	track := model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"}
	created := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{Track: track, RemoteURL: "http://8.8.8.8/expired"})
	var session model.CreateSessionResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("session create = %d: %s", created.Code, created.Body.String())
	}
	waitServerTaskState(t, fixture.manager, session.TaskID, model.TaskNeedsURL)
	response := serveJSONAuthorized(fixture.handler, http.MethodPut, "/v1/tasks/"+session.TaskID+"/url", map[string]string{"remoteUrl": "http://8.8.8.8/refreshed"})
	if response.Code != http.StatusOK {
		t.Fatalf("URL refresh = %d: %s", response.Code, response.Body.String())
	}
	waitServerTaskState(t, fixture.manager, session.TaskID, model.TaskCompleted)
}

func TestTaskURLRefreshRouteRecoversGenuinelyFailedCacheTask(t *testing.T) {
	var requests atomic.Int32
	media := []byte("recovered media")
	client := handlerTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = writer.Write([]byte("range ignored"))
			return
		}
		serveRangeMedia(writer, request, media)
	}))
	fixture := newTestFixture(t, Options{HTTPClient: client})
	defer fixture.cleanup()
	track := model.TrackInfo{Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3"}
	created := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{Track: track, RemoteURL: "http://8.8.8.8/fatal"})
	var session model.CreateSessionResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("session create = %d: %s", created.Code, created.Body.String())
	}
	waitServerTaskState(t, fixture.manager, session.TaskID, model.TaskFailed)
	response := serveJSONAuthorized(fixture.handler, http.MethodPut, "/v1/tasks/"+session.TaskID+"/url", map[string]string{"remoteUrl": "http://8.8.8.8/fresh"})
	if response.Code != http.StatusOK {
		t.Fatalf("failed-task URL refresh = %d: %s", response.Code, response.Body.String())
	}
	waitServerTaskState(t, fixture.manager, session.TaskID, model.TaskCompleted)
}

func TestFirstPlaybackCachesAndSecondPlaybackStreamsWithoutUpstream(t *testing.T) {
	media := make([]byte, 512*1024)
	for offset := range media {
		media[offset] = byte(offset % 251)
	}
	upstream, recorder := testserver.NewRangeServer(media)
	t.Cleanup(upstream.Close)
	recorder.SetChunkSize(8 * 1024)
	recorder.SetChunkDelay(time.Millisecond)
	fixture := newTestFixture(t, Options{
		HTTPClient: rewrittenTestClient(t, upstream.URL),
		Config:     model.Config{CacheLimitBytes: 2 * int64(len(media))},
	})
	defer fixture.cleanup()
	track := model.TrackInfo{
		Hash: testHash, CatalogHash: testHash, RequestedQuality: "320", Quality: "320",
		Effect: "none", Extension: "mp3", Title: "Integrated Song", Artist: "Echo",
	}
	requestBody := model.CreateSessionRequest{Track: track, RemoteURL: "http://8.8.8.8/song.mp3"}
	firstCreated := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", requestBody)
	var firstSession model.CreateSessionResponse
	if firstCreated.Code != http.StatusOK || json.Unmarshal(firstCreated.Body.Bytes(), &firstSession) != nil {
		t.Fatalf("first session create = %d: %s", firstCreated.Code, firstCreated.Body.String())
	}
	if tasks := fixture.manager.Tasks(); len(tasks) != 1 || tasks[0].Kind != model.TaskKindCache {
		t.Fatalf("first playback tasks = %+v, want one cache task", tasks)
	}
	firstPlayed := servePlaybackRange(t, fixture.handler, firstSession.PlayURL, "bytes=0-")
	if firstPlayed.Code != http.StatusPartialContent || !bytes.Equal(firstPlayed.Body.Bytes(), media) {
		t.Fatalf("first playback = %d, bytes = %d", firstPlayed.Code, firstPlayed.Body.Len())
	}
	waitServerTaskState(t, fixture.manager, firstSession.TaskID, model.TaskCompleted)
	key, _ := model.CacheKey(testHash, "320", "none")
	entry, found, _ := fixture.index.Lookup(key)
	if !found || !entry.Complete || entry.Size != int64(len(media)) {
		t.Fatalf("first playback cache = %+v, found = %t", entry, found)
	}
	if got := recorder.RequestCount(); got != 1 {
		t.Fatalf("first playback opened %d upstream requests, want 1", got)
	}

	beforeReplay := recorder.RequestCount()
	upstream.Close()
	secondCreated := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", requestBody)
	var secondSession model.CreateSessionResponse
	if secondCreated.Code != http.StatusOK || json.Unmarshal(secondCreated.Body.Bytes(), &secondSession) != nil {
		t.Fatalf("second session create = %d: %s", secondCreated.Code, secondCreated.Body.String())
	}
	secondPlayed := servePlaybackRange(t, fixture.handler, secondSession.PlayURL, "bytes=0-")
	if secondPlayed.Code != http.StatusPartialContent || !bytes.Equal(secondPlayed.Body.Bytes(), media) {
		t.Fatalf("second playback = %d, bytes = %d", secondPlayed.Code, secondPlayed.Body.Len())
	}
	if got := recorder.RequestCount(); got != beforeReplay {
		t.Fatalf("second playback opened %d new upstream requests", got-beforeReplay)
	}
}

func TestActivePlaybackAndManualDownloadShareCacheTaskAndExportOnce(t *testing.T) {
	media := make([]byte, 4*1024*1024)
	for offset := range media {
		media[offset] = byte(offset % 251)
	}
	upstream, recorder := testserver.NewRangeServer(media)
	t.Cleanup(upstream.Close)
	recorder.SetChunkSize(8 * 1024)
	recorder.SetChunkDelay(time.Millisecond)
	downloadRoot := t.TempDir()
	fixture := newTestFixture(t, Options{
		HTTPClient: rewrittenTestClient(t, upstream.URL),
		Config:     model.Config{DownloadRoot: downloadRoot, CacheLimitBytes: 2 * int64(len(media))},
	})
	defer fixture.cleanup()
	track := model.TrackInfo{
		Hash: "22222222222222222222222222222222", CatalogHash: "22222222222222222222222222222222",
		RequestedQuality: "320", Quality: "320", Effect: "none", Extension: "mp3",
		Title: "Shared Song", Artist: "Echo", DurationSeconds: 240,
	}
	requestBody := model.CreateSessionRequest{Track: track, RemoteURL: "http://8.8.8.8/shared.mp3"}
	created := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", requestBody)
	var session model.CreateSessionResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("session create = %d: %s", created.Code, created.Body.String())
	}
	blocked := newBlockingResponseWriter()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(blocked.release) })
	streamDone := make(chan struct{})
	request := playbackRequest(t, session.PlayURL, "bytes=0-")
	go func() {
		defer close(streamDone)
		fixture.handler.ServeHTTP(blocked, request)
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback stream did not become active")
	}

	key, _ := model.CacheKey(track.Hash, "320", "none")
	downloadResponse := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/downloads", map[string]string{"key": key})
	var download model.Task
	if downloadResponse.Code != http.StatusAccepted || json.Unmarshal(downloadResponse.Body.Bytes(), &download) != nil {
		t.Fatalf("download create = %d: %s", downloadResponse.Code, downloadResponse.Body.String())
	}
	var cacheTasks, downloadTasks int
	for _, task := range fixture.manager.Tasks() {
		if task.Track.Key != key {
			continue
		}
		switch task.Kind {
		case model.TaskKindCache:
			cacheTasks++
		case model.TaskKindDownload:
			downloadTasks++
		}
	}
	if cacheTasks != 1 || downloadTasks != 1 {
		t.Fatalf("shared task counts = cache:%d download:%d; tasks = %+v", cacheTasks, downloadTasks, fixture.manager.Tasks())
	}
	releaseOnce.Do(func() { close(blocked.release) })
	<-streamDone
	waitServerTaskState(t, fixture.manager, download.ID, model.TaskCompleted)
	var completed model.Task
	for _, task := range fixture.manager.Tasks() {
		if task.ID == download.ID {
			completed = task
		}
	}
	if completed.OutputPath == "" {
		t.Fatalf("completed download has no output path: %+v", completed)
	}
	exported, err := os.ReadFile(completed.OutputPath)
	if err != nil || !bytes.Equal(exported, media) {
		t.Fatalf("exported file bytes = %d, err = %v", len(exported), err)
	}
	if got := recorder.RequestCount(); got != 1 {
		t.Fatalf("shared playback and download opened %d upstream requests, want 1", got)
	}
	files, err := os.ReadDir(downloadRoot)
	if err != nil {
		t.Fatal(err)
	}
	regularFiles := 0
	for _, file := range files {
		if !file.IsDir() {
			regularFiles++
		}
	}
	if regularFiles != 1 {
		t.Fatalf("download root contains %d files, want exactly 1: %+v", regularFiles, files)
	}
}

func TestLimitReductionUsesLowWaterMarkAndProtectsCurrentPlayback(t *testing.T) {
	fixture := newTestFixture(t, Options{Config: model.Config{CacheLimitBytes: 1 << 20}})
	defer fixture.cleanup()
	active := addCompletedServerEntry(t, fixture.index, "1000000000000001", bytes.Repeat([]byte("a"), 80))
	firstInactive := addCompletedServerEntry(t, fixture.index, "1000000000000002", bytes.Repeat([]byte("b"), 15))
	secondInactive := addCompletedServerEntry(t, fixture.index, "1000000000000003", bytes.Repeat([]byte("c"), 15))
	now := time.Now().UTC()
	if err := fixture.index.Touch(active.Key, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.index.Touch(firstInactive.Key, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.index.Touch(secondInactive.Key, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	created := serveJSONAuthorized(fixture.handler, http.MethodPost, "/v1/proxy/sessions", model.CreateSessionRequest{
		Track: active, RemoteURL: "http://8.8.8.8/already-cached.mp3",
	})
	var session model.CreateSessionResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("cached session create = %d: %s", created.Code, created.Body.String())
	}
	blocked := newBlockingResponseWriter()
	request := playbackRequest(t, session.PlayURL, "bytes=0-")
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		fixture.handler.ServeHTTP(blocked, request)
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("cached playback did not start")
	}

	config := model.Config{
		CacheRoot: fixture.index.Root(), DownloadRoot: t.TempDir(), CacheLimitBytes: 100,
	}
	response := serveJSONAuthorized(fixture.handler, http.MethodPut, "/v1/config", config)
	close(blocked.release)
	<-streamDone
	if response.Code != http.StatusOK {
		t.Fatalf("limit update = %d: %s", response.Code, response.Body.String())
	}
	view, err := fixture.manager.CacheView()
	if err != nil || view.CacheBytes != 80 {
		t.Fatalf("cache after cleanup = %+v, err = %v", view, err)
	}
	if lookup, err := fixture.manager.Lookup(model.CacheLookupRequest{Key: active.Key}); err != nil || lookup.Status != "complete" {
		t.Fatalf("active playback cache was not protected: %+v, %v", lookup, err)
	}
	for _, track := range []model.TrackInfo{firstInactive, secondInactive} {
		if lookup, err := fixture.manager.Lookup(model.CacheLookupRequest{Key: track.Key}); err != nil || lookup.Status != "miss" {
			t.Fatalf("inactive cache %s remains: %+v, %v", track.Key, lookup, err)
		}
	}
}

type testFixture struct {
	handler http.Handler
	manager *proxy.Manager
	index   *cache.Index
	cleanup func()
}

func newTestHandler(t *testing.T, overrides Options) (http.Handler, func()) {
	t.Helper()
	fixture := newTestFixture(t, overrides)
	return fixture.handler, fixture.cleanup
}

func newTestFixture(t *testing.T, overrides Options) testFixture {
	t.Helper()
	cacheRoot := overrides.Config.CacheRoot
	if cacheRoot == "" {
		cacheRoot = filepath.Join(t.TempDir(), "cache")
	}
	index, err := cache.Open(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	client := overrides.HTTPClient
	if client == nil {
		client = rangeTestClient(t, []byte("default media bytes"))
	}
	manager := proxy.NewManagerForTesting(index, client)
	config := overrides.Config
	config.CacheRoot = cacheRoot
	if config.DownloadRoot == "" {
		config.DownloadRoot = t.TempDir()
	}
	if config.CacheLimitBytes == 0 {
		config.CacheLimitBytes = 1 << 20
	}
	if err := manager.SetDownloadRoots(config.DownloadRoot, config.ManagedDownloadRoots); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDownloadRecords(config.CompletedDownloadPaths); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetLimit(config.CacheLimitBytes); err != nil {
		t.Fatal(err)
	}
	overrides.Token = testBearer
	overrides.Manager = manager
	overrides.Config = config
	overrides.Version = "test"
	return testFixture{handler: New(overrides), manager: manager, index: index, cleanup: func() { _ = manager.Close() }}
}

func addCompletedServerEntry(t *testing.T, index *cache.Index, hash string, contents []byte) model.TrackInfo {
	t.Helper()
	key, err := model.CacheKey(hash, "320", "none")
	if err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{
		Key: key, Hash: hash, CatalogHash: hash, RequestedQuality: "320", Quality: "320",
		Effect: "none", Extension: "mp3", Title: hash,
	}
	relative := "audio/" + key + ".mp3"
	file, err := index.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := index.Upsert(key, cache.Entry{
		Track: track, RelativePath: relative, Size: int64(len(contents)), TotalBytes: int64(len(contents)),
		Ranges: []model.ByteRange{{Start: 0, End: int64(len(contents))}}, Complete: true,
		CreatedAt: now, CompletedAt: now, LastAccessedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return track
}

func addPartialServerEntry(t *testing.T, index *cache.Index, hash string, contents []byte) model.TrackInfo {
	t.Helper()
	key, err := model.CacheKey(hash, "320", "none")
	if err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{Key: key, Hash: hash, Quality: "320", Effect: "none", Extension: "mp3"}
	relative := "temp/" + key + ".part"
	file, err := index.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := index.Upsert(key, cache.Entry{
		Track: track, RelativePath: relative, Size: int64(len(contents)), TotalBytes: int64(len(contents) * 2),
		Ranges: []model.ByteRange{{Start: 0, End: int64(len(contents))}}, CreatedAt: now, LastAccessedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return track
}

func waitServerTaskState(t *testing.T, manager *proxy.Manager, id string, state model.TaskState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, task := range manager.Tasks() {
			if task.ID == id && task.State == state {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %q did not reach %q: %+v", id, state, manager.Tasks())
}

func serveAuthorized(handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	return serveRequest(handler, method, path, body, "Bearer "+testBearer)
}

func serveJSONAuthorized(handler http.Handler, method, path string, value any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(value)
	return serveAuthorized(handler, method, path, bytes.NewReader(body))
}

func serveRequest(handler http.Handler, method, path string, body io.Reader, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, body)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", authorization)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func servePlaybackRange(t *testing.T, handler http.Handler, playURL, byteRange string) *httptest.ResponseRecorder {
	t.Helper()
	request := playbackRequest(t, playURL, byteRange)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func playbackRequest(t *testing.T, playURL, byteRange string) *http.Request {
	t.Helper()
	parsed, err := url.Parse(playURL)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+parsed.RequestURI(), nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Range", byteRange)
	return request
}

func handlerTestClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return rewrittenTestClient(t, server.URL)
}

func rangeTestClient(t *testing.T, media []byte) *http.Client {
	t.Helper()
	return handlerTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		serveRangeMedia(writer, request, media)
	}))
}

func serveRangeMedia(writer http.ResponseWriter, request *http.Request, media []byte) {
	response, err := rangeTransport(media).RoundTrip(request)
	if err != nil {
		http.Error(writer, "test upstream failed", http.StatusInternalServerError)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func rewrittenTestClient(t *testing.T, targetURL string) *http.Client {
	t.Helper()
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	if target.Scheme != "http" || target.Host == "" {
		t.Fatalf("unsupported rewritten test target %q", targetURL)
	}
	dialer := &net.Dialer{}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target.Host)
		},
	}}
}

type blockingResponseWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingResponseWriter() *blockingResponseWriter {
	return &blockingResponseWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
}

func (writer *blockingResponseWriter) Header() http.Header { return writer.header }
func (writer *blockingResponseWriter) WriteHeader(int)     {}
func (writer *blockingResponseWriter) Write(content []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(content), nil
}

func assertJSONErrorHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type = %q", got)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %v", response.Header())
	}
	var payload ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Code == "" || payload.Message == "" {
		t.Fatalf("error payload = %q, %v", response.Body.String(), err)
	}
}

func rangeTransport(media []byte) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested, err := cache.ParseSingleRange(request.Header.Get("Range"), int64(len(media)))
		if err != nil {
			return &http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		header := make(http.Header)
		header.Set("Content-Type", "audio/mpeg")
		header.Set("Accept-Ranges", "bytes")
		header.Set("Content-Length", strconv.FormatInt(requested.End-requested.Start, 10))
		header.Set("Content-Range", "bytes "+strconv.FormatInt(requested.Start, 10)+"-"+strconv.FormatInt(requested.End-1, 10)+"/"+strconv.Itoa(len(media)))
		return &http.Response{
			StatusCode: http.StatusPartialContent, Status: "206 Partial Content", Header: header,
			Body: io.NopCloser(bytes.NewReader(media[requested.Start:requested.End])), ContentLength: requested.End - requested.Start, Request: request,
		}, nil
	})
}
