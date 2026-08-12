package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/testserver"
)

const testRemoteURL = "http://media.example.test/song.mp3"

func TestRequestCachesCompleteFile(t *testing.T) {
	fixture := newProxyFixture(t)
	session := fixture.create(t)

	response := serveRange(t, session, "bytes=0-")
	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.Code)
	}
	if got := response.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if got := response.Header().Get("Content-Range"); got != "bytes 0-4194303/4194304" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "4194304" {
		t.Fatalf("Content-Length = %q", got)
	}
	if got := response.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !bytes.Equal(response.Body.Bytes(), fixture.data) {
		t.Fatal("response bytes do not match upstream")
	}

	entry := waitForEntry(t, fixture.index, fixture.key, true)
	if entry.Size != int64(len(fixture.data)) || entry.TotalBytes != int64(len(fixture.data)) {
		t.Fatalf("complete entry sizes = %d/%d", entry.Size, entry.TotalBytes)
	}
	file, err := fixture.index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(cached, fixture.data) {
		t.Fatal("completed cache file does not match upstream")
	}
	lookup, err := fixture.manager.Lookup(model.CacheLookupRequest{
		CatalogHash: fixture.request.Track.CatalogHash, RequestedQuality: "320", Effect: "none",
	})
	if err != nil || lookup.Status != "complete" || lookup.Key != fixture.key || lookup.Path != entry.RelativePath {
		t.Fatalf("offline alias lookup = %#v, err = %v", lookup, err)
	}
}

func TestConcurrentCreateOrReuseDeduplicatesByActualKey(t *testing.T) {
	fixture := newProxyFixture(t)
	const callers = 16
	ids := make(chan string, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := fixture.request
			request.Track.Key = fmt.Sprintf("untrusted-%d", index)
			session, err := fixture.manager.CreateOrReuse(context.Background(), request)
			if err != nil {
				errors <- err
				return
			}
			ids <- session.ID()
		}(index)
	}
	group.Wait()
	close(ids)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("session ID = %q, want %q", id, first)
		}
	}
	if got := len(fixture.manager.Tasks()); got != 1 {
		t.Fatalf("task count = %d, want 1", got)
	}
}

func TestCachedRangeDoesNotOpenAnotherUpstreamRequest(t *testing.T) {
	fixture := newProxyFixture(t)
	session := fixture.create(t)
	first := serveRange(t, session, "bytes=0-524287")
	if !bytes.Equal(first.Body.Bytes(), fixture.data[:512*1024]) {
		t.Fatal("first cached range is corrupt")
	}
	count := fixture.recorder.RequestCount()
	second := serveRange(t, session, "bytes=0-524287")
	if !bytes.Equal(second.Body.Bytes(), fixture.data[:512*1024]) {
		t.Fatal("second cached range is corrupt")
	}
	if got := fixture.recorder.RequestCount(); got != count {
		t.Fatalf("upstream requests = %d after cached read, want %d", got, count)
	}
}

func TestSeekReprioritizesWithoutConcurrentProducer(t *testing.T) {
	fixture := newProxyFixture(t)
	fixture.recorder.SetChunkDelay(3 * time.Millisecond)
	fixture.recorder.SetChunkSize(32 * 1024)
	session := fixture.create(t)
	waitFor(t, 2*time.Second, func() bool { return fixture.recorder.RequestCount() == 1 }, "initial upstream request")

	response := serveRange(t, session, "bytes=2097152-")
	if !bytes.Equal(response.Body.Bytes(), fixture.data[2*1024*1024:]) {
		t.Fatal("seek response is corrupt")
	}
	waitFor(t, 2*time.Second, func() bool { return fixture.recorder.RequestCount() >= 2 }, "prioritized request")
	ranges := fixture.recorder.RequestedIntervals()
	if ranges[1].Start != 2*1024*1024 {
		t.Fatalf("second request starts at %d, want 2097152; all ranges: %#v", ranges[1].Start, ranges)
	}
	backward := serveRange(t, session, "bytes=0-1048575")
	if backward.Code != http.StatusPartialContent || !bytes.Equal(backward.Body.Bytes(), fixture.data[:1024*1024]) {
		t.Fatal("backward seek response is corrupt")
	}
	entry := waitForEntry(t, fixture.index, fixture.key, true)
	file, err := fixture.index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(cached, fixture.data) {
		t.Fatal("seeked playback did not fill an exact complete cache")
	}
	if fixture.transport.MaxActive() > 1 {
		t.Fatalf("observed %d concurrent upstream producers", fixture.transport.MaxActive())
	}
}

func TestInterruptedUpstreamResumesAfterURLUpdate(t *testing.T) {
	fixture := newProxyFixture(t)
	fixture.recorder.SetChunkDelay(time.Millisecond)
	fixture.recorder.DisconnectNextAfter(256 * 1024)
	fixture.create(t)
	waitFor(t, 2*time.Second, func() bool {
		entry, ok, _ := fixture.index.Lookup(fixture.key)
		return ok && len(entry.Ranges) > 0 && entry.Ranges[0].End > 0
	}, "partial checkpoint after disconnect")
	fixture.recorder.FailNext403()

	waitFor(t, 3*time.Second, func() bool {
		tasks := fixture.manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskNeedsURL
	}, "needs-url after interrupted upstream")
	partial := waitForEntry(t, fixture.index, fixture.key, false)
	if len(partial.Ranges) == 0 || partial.Ranges[0].End == 0 {
		t.Fatalf("partial ranges were not checkpointed: %#v", partial.Ranges)
	}
	missingStart := partial.Ranges[0].End
	if _, err := fixture.index.StatFile("temp/" + fixture.key + ".part"); err != nil {
		t.Fatalf("partial file is missing: %v", err)
	}

	if err := fixture.manager.UpdateURL(fixture.key, testRemoteURL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		tasks := fixture.manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskCompleted
	}, "completion after URL update")
	entry := waitForEntry(t, fixture.index, fixture.key, true)
	if entry.Size != int64(len(fixture.data)) {
		t.Fatalf("completed size = %d", entry.Size)
	}
	requested := fixture.recorder.RequestedIntervals()
	resumedAtMissingByte := false
	for _, interval := range requested[1:] {
		if interval.Start == missingStart {
			resumedAtMissingByte = true
			break
		}
	}
	if !resumedAtMissingByte {
		t.Fatalf("upstream did not resume at missing byte %d: %#v", missingStart, requested)
	}
	file, err := fixture.index.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(cached, fixture.data) {
		t.Fatal("resumed cache does not match upstream bytes")
	}
}

func TestTwoReadersShareGrowingCacheWithoutCorruption(t *testing.T) {
	fixture := newProxyFixture(t)
	fixture.recorder.SetChunkDelay(2 * time.Millisecond)
	fixture.recorder.SetChunkSize(32 * 1024)
	session := fixture.create(t)

	responses := make(chan *httptest.ResponseRecorder, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			responses <- serveRange(t, session, "bytes=0-")
		}()
	}
	group.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), fixture.data) {
			t.Fatal("concurrent reader received corrupt bytes")
		}
	}
	if fixture.transport.MaxActive() > 1 {
		t.Fatalf("observed %d concurrent upstream producers", fixture.transport.MaxActive())
	}
}

type proxyFixture struct {
	data      []byte
	server    *httptest.Server
	recorder  *testserver.RangeRecorder
	index     *cache.Index
	manager   *Manager
	transport *rewriteTransport
	request   model.CreateSessionRequest
	key       string
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	data := make([]byte, 4*1024*1024)
	for index := range data {
		data[index] = byte(index % 251)
	}
	server, recorder := testserver.NewRangeServer(data)
	t.Cleanup(server.Close)
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	transport := &rewriteTransport{target: mustParseURL(t, server.URL), base: http.DefaultTransport}
	client := &http.Client{Transport: transport}
	manager := NewManager(index, nil)
	manager.client = client
	t.Cleanup(func() { _ = manager.Close() })
	track := model.TrackInfo{
		Key:              "supplied-key-is-not-trusted",
		CatalogHash:      "11111111111111111111111111111111",
		Hash:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RequestedQuality: "320",
		Quality:          "320",
		Effect:           "none",
		Extension:        "mp3",
		DurationSeconds:  240,
	}
	key, err := model.CacheKey(track.Hash, track.Quality, track.Effect)
	if err != nil {
		t.Fatal(err)
	}
	return &proxyFixture{
		data: data, server: server, recorder: recorder, index: index, manager: manager, transport: transport,
		request: model.CreateSessionRequest{Track: track, RemoteURL: testRemoteURL}, key: key,
	}
}

func (fixture *proxyFixture) create(t *testing.T) *Session {
	t.Helper()
	session, err := fixture.manager.CreateOrReuse(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

type rewriteTransport struct {
	target    *url.URL
	base      http.RoundTripper
	mu        sync.Mutex
	active    int
	maxActive int
}

func (transport *rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = transport.target.Scheme
	clone.URL.Host = transport.target.Host
	clone.Host = transport.target.Host
	response, err := transport.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	transport.mu.Lock()
	transport.active++
	if transport.active > transport.maxActive {
		transport.maxActive = transport.active
	}
	transport.mu.Unlock()
	response.Body = &trackedBody{ReadCloser: response.Body, onClose: func() {
		transport.mu.Lock()
		transport.active--
		transport.mu.Unlock()
	}}
	return response, nil
}

func (transport *rewriteTransport) MaxActive() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.maxActive
}

type trackedBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (body *trackedBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.onClose)
	return err
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func serveRange(t *testing.T, session *Session, byteRange string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/stream", nil)
	request.Header.Set("Range", byteRange)
	response := httptest.NewRecorder()
	session.ServeHTTP(response, request)
	return response
}

func waitForEntry(t *testing.T, index *cache.Index, key string, complete bool) cache.Entry {
	t.Helper()
	var found cache.Entry
	waitFor(t, 5*time.Second, func() bool {
		entry, ok, _ := index.Lookup(key)
		if ok && entry.Complete == complete {
			found = entry
			return true
		}
		return false
	}, fmt.Sprintf("cache entry complete=%t", complete))
	return found
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestRangeResponsesRejectMalformedOrUnsatisfiableRanges(t *testing.T) {
	fixture := newProxyFixture(t)
	session := fixture.create(t)
	for _, header := range []string{"", "items=0-1", "bytes=0-1,3-4", "bytes=999999999-"} {
		t.Run(strings.ReplaceAll(header, "/", "_"), func(t *testing.T) {
			response := serveRange(t, session, header)
			if response.Code != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d for %q", response.Code, header)
			}
			if got := response.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes */") {
				t.Fatalf("Content-Range = %q", got)
			}
		})
	}
}

func TestManagerHydratesIncompleteEntryWithoutPersistingRemoteURL(t *testing.T) {
	data := []byte(strings.Repeat("range-cache", 64*1024))
	server, recorder := testserver.NewRangeServer(data)
	t.Cleanup(server.Close)
	root := t.TempDir()
	index, err := cache.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	transport := &rewriteTransport{target: mustParseURL(t, server.URL), base: http.DefaultTransport}
	client := &http.Client{Transport: transport}
	manager := NewManager(index, nil)
	manager.client = client
	recorder.SetChunkSize(8 * 1024)
	recorder.SetChunkDelay(time.Millisecond)
	recorder.DisconnectNextAfter(128 * 1024)
	track := model.TrackInfo{Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Quality: "320", Effect: "none", Extension: "mp3"}
	key, _ := model.CacheKey(track.Hash, track.Quality, track.Effect)
	if _, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{Track: track, RemoteURL: testRemoteURL}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		entry, ok, _ := index.Lookup(key)
		return ok && len(entry.Ranges) > 0 && entry.Ranges[0].End > 0
	}, "partial checkpoint before restart")
	recorder.FailNext403()
	waitFor(t, 2*time.Second, func() bool {
		tasks := manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskNeedsURL
	}, "initial needs-url state")
	partial, ok, _ := index.Lookup(key)
	if !ok || len(partial.Ranges) == 0 {
		t.Fatalf("partial entry before restart = %#v, found = %t", partial, ok)
	}
	missingStart := partial.Ranges[0].End
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(testRemoteURL)) || bytes.Contains(stored, []byte("media.example.test")) {
		t.Fatal("remote URL leaked into cache index")
	}

	reopened, err := cache.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(reopened, nil)
	restarted.client = client
	for _, session := range restarted.sessions {
		session.client = client
	}
	t.Cleanup(func() { _ = restarted.Close() })
	tasks := restarted.Tasks()
	if len(tasks) != 1 || tasks[0].State != model.TaskNeedsURL || tasks[0].Track.Key != key {
		t.Fatalf("hydrated tasks = %#v", tasks)
	}
	requestsBeforeURL := recorder.RequestCount()
	if err := restarted.UpdateURL(key, testRemoteURL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		tasks := restarted.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskCompleted
	}, "restarted completion after fresh URL")
	entry := waitForEntry(t, reopened, key, true)
	file, err := reopened.OpenFile(entry.RelativePath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(cached, data) {
		t.Fatal("restarted cache does not match upstream bytes")
	}
	requested := recorder.RequestedIntervals()
	if recorder.RequestCount() <= requestsBeforeURL || requested[requestsBeforeURL].Start != missingStart {
		t.Fatalf("fresh URL resumed at %#v, want start %d after %d requests", requested, missingStart, requestsBeforeURL)
	}
}

func TestSecureClientValidatesRedirectsAndStripsCrossHostCredentials(t *testing.T) {
	client := secureHTTPClient(nil)
	redirect := &http.Request{
		URL:    mustParseURL(t, "https://cdn.example.test/song.mp3"),
		Header: http.Header{"Authorization": {"Bearer secret"}, "Cookie": {"token=secret"}},
	}
	via := []*http.Request{{URL: mustParseURL(t, "https://media.example.test/song.mp3")}}
	if err := client.CheckRedirect(redirect, via); err != nil {
		t.Fatal(err)
	}
	if redirect.Header.Get("Authorization") != "" || redirect.Header.Get("Cookie") != "" {
		t.Fatalf("sensitive redirect headers were preserved: %#v", redirect.Header)
	}
	privateTarget := &http.Request{URL: mustParseURL(t, "http://127.0.0.1/song.mp3"), Header: make(http.Header)}
	if err := client.CheckRedirect(privateTarget, via); err == nil {
		t.Fatal("private redirect target was accepted")
	}
}

func TestPlaybackTokenAndProtectedKeyLifecycle(t *testing.T) {
	fixture := newProxyFixture(t)
	session := fixture.create(t)
	if len(session.PlaybackToken()) != 64 {
		t.Fatalf("playback token length = %d, want 64", len(session.PlaybackToken()))
	}
	if found, ok := fixture.manager.Session(session.ID(), session.PlaybackToken()); !ok || found != session {
		t.Fatal("valid session ID and playback token were rejected")
	}
	if _, ok := fixture.manager.Session(session.ID(), "wrong"); ok {
		t.Fatal("invalid playback token was accepted")
	}
	if !fixture.manager.ProtectedKeys()[fixture.key] {
		t.Fatal("active producer key is not protected")
	}
	serveRange(t, session, "bytes=0-")
	waitFor(t, 2*time.Second, func() bool {
		return !fixture.manager.ProtectedKeys()[fixture.key]
	}, "completed idle session to become unprotected")
}
