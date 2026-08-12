package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/testserver"
)

func TestRealCrossHostRedirectDoesNotForwardSignedReferer(t *testing.T) {
	type observedRequest struct {
		referer            string
		authorization      string
		proxyAuthorization string
		cookie             string
		query              string
		userinfo           string
	}
	observed := make(chan observedRequest, 1)
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := observedRequest{
			referer: request.Header.Get("Referer"), authorization: request.Header.Get("Authorization"),
			proxyAuthorization: request.Header.Get("Proxy-Authorization"),
			cookie:             request.Header.Get("Cookie"), query: request.URL.RawQuery,
		}
		if request.URL.User != nil {
			got.userinfo = request.URL.User.String()
		}
		observed <- got
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://redirect-user:redirect-secret@redirect-target.example.test/final", http.StatusFound)
	}))
	defer first.Close()

	resolverAddresses := map[string]netip.Addr{
		"redirect-source.example.test": netip.MustParseAddr("8.8.8.8"),
		"redirect-target.example.test": netip.MustParseAddr("1.1.1.1"),
	}
	dialTargets := map[string]string{
		"8.8.8.8:80": mustParseURL(t, first.URL).Host,
		"1.1.1.1:80": mustParseURL(t, second.URL).Host,
	}
	dialer := &net.Dialer{}
	client := secureHTTPClientWithNetwork(nil,
		func(_ context.Context, _, host string) ([]netip.Addr, error) {
			address, ok := resolverAddresses[host]
			if !ok {
				return nil, fmt.Errorf("no test address for %q", host)
			}
			return []netip.Addr{address}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			target, ok := dialTargets[address]
			if !ok {
				return nil, fmt.Errorf("no test target for %q", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
	)
	request, err := http.NewRequest(http.MethodGet, "http://redirect-source.example.test/song?signature=top-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer top-secret")
	request.Header.Set("Proxy-Authorization", "Basic top-secret")
	request.Header.Set("Cookie", "session=top-secret")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	got := <-observed
	if got.referer != "" || got.authorization != "" || got.proxyAuthorization != "" || got.cookie != "" || got.query != "" || got.userinfo != "" {
		t.Fatalf("redirect leaked request data: %#v", got)
	}
}

func TestStalledResponseHeadersRetryThenFailWithoutHandlerLeak(t *testing.T) {
	var requests atomic.Int64
	var active atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		active.Add(1)
		defer active.Add(-1)
		<-request.Context().Done()
	}))
	defer server.Close()
	options := fastFailureOptions()
	options.responseHeaderTimeout = 20 * time.Millisecond
	manager, index := newReviewManager(t, server.URL, options)
	defer index.Close()
	defer manager.Close()
	createReviewSession(t, manager)
	waitFor(t, 2*time.Second, func() bool {
		tasks := manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskFailed
	}, "header timeout failure")
	waitFor(t, time.Second, func() bool { return active.Load() == 0 }, "stalled header handlers to exit")
	if requests.Load() != 5 {
		t.Fatalf("upstream requests = %d, want 5", requests.Load())
	}
}

func TestStalledResponseBodyRetryThenFailWithoutHandlerLeak(t *testing.T) {
	const total = int64(4096)
	var requests atomic.Int64
	var active atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		active.Add(1)
		defer active.Add(-1)
		requested, err := cache.ParseSingleRange(request.Header.Get("Range"), total)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.Start, requested.End-1, total))
		writer.Header().Set("Content-Length", strconv.FormatInt(requested.End-requested.Start, 10))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(bytes.Repeat([]byte{0x5a}, 16))
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	options := fastFailureOptions()
	options.bodyIdleTimeout = 20 * time.Millisecond
	manager, index := newReviewManager(t, server.URL, options)
	defer index.Close()
	defer manager.Close()
	createReviewSession(t, manager)
	waitFor(t, 2*time.Second, func() bool {
		tasks := manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskFailed
	}, "body idle timeout failure")
	waitFor(t, time.Second, func() bool { return active.Load() == 0 }, "stalled body handlers to exit")
	if requests.Load() != 5 {
		t.Fatalf("upstream requests = %d, want 5", requests.Load())
	}
}

func TestSeekDoesNotBypassRetryDeadline(t *testing.T) {
	requestSeen := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestSeen <- struct{}{}
		http.Error(writer, "retry", http.StatusInternalServerError)
	}))
	defer server.Close()
	type delayCall struct {
		delay   time.Duration
		release chan time.Time
	}
	delays := make(chan delayCall, 4)
	options := defaultSessionOptions()
	options.jitter = func(delay time.Duration) time.Duration { return delay }
	options.after = func(delay time.Duration) <-chan time.Time {
		release := make(chan time.Time, 1)
		delays <- delayCall{delay: delay, release: release}
		return release
	}
	manager, index := newReviewManager(t, server.URL, options)
	defer index.Close()
	defer manager.Close()
	session := createReviewSession(t, manager)
	<-requestSeen
	wantDelays := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	for index, want := range wantDelays {
		call := <-delays
		if call.delay != want {
			t.Fatalf("retry delay %d = %s, want %s", index, call.delay, want)
		}
		if index == 0 {
			session.requestDemand(2048, 3072)
			select {
			case <-requestSeen:
				t.Fatal("seek bypassed active retry deadline")
			case <-time.After(40 * time.Millisecond):
			}
		}
		call.release <- time.Now()
		<-requestSeen
	}
	waitFor(t, time.Second, func() bool {
		tasks := manager.Tasks()
		return len(tasks) == 1 && tasks[0].State == model.TaskFailed
	}, "retry exhaustion")
}

func TestHydratedTerminalPartialReturnsErrorBefore206(t *testing.T) {
	manager, index, session := hydratedSession(t, []byte("cached"), 128, []model.ByteRange{{Start: 0, End: 6}})
	defer index.Close()
	defer manager.Close()
	response := serveRange(t, session, "bytes=0-")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; headers = %#v", response.Code, response.Header())
	}
	if response.Header().Get("Content-Range") != "" {
		t.Fatalf("terminal partial emitted 206 headers: %#v", response.Header())
	}
}

func TestServeHTTPStartupDeadlineRequiresRequestedStartByte(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	options := defaultSessionOptions()
	options.startupWait = 20 * time.Millisecond
	manager := newManagerWithOptions(index, nil, options)
	defer manager.Close()
	track := model.TrackInfo{Hash: "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc", Quality: "320", Effect: "none", Extension: "mp3"}
	key, track, err := normalizeTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSession(manager, key, cache.Entry{
		Track: track, RelativePath: partialPath(key), TotalBytes: 128, CreatedAt: time.Now().UTC(),
	}, testRemoteURL, model.TaskBuffering)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close(false)
	response := serveRange(t, session, "bytes=0-")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("zero-byte startup status = %d, want 503", response.Code)
	}
	if response.Header().Get("Content-Range") != "" {
		t.Fatalf("zero-byte startup committed range headers: %#v", response.Header())
	}
}

func TestCanceledKeyCanCreateUsableReplacementSession(t *testing.T) {
	fixture := newProxyFixture(t)
	first := fixture.create(t)
	if err := fixture.manager.Cancel(first.ID()); err != nil {
		t.Fatal(err)
	}
	second := fixture.create(t)
	if second.ID() == first.ID() {
		t.Fatal("canceled session ID was reused")
	}
	response := serveRange(t, second, "bytes=0-")
	if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), fixture.data) {
		t.Fatal("replacement session did not resume the partial cache")
	}
	var sawCanceled bool
	for _, task := range fixture.manager.Tasks() {
		if task.ID == first.ID() && task.State == model.TaskCanceled {
			sawCanceled = true
		}
	}
	if !sawCanceled {
		t.Fatal("canceled task history was not retained")
	}
}

func TestReplacementWaitsForCanceledSessionShutdownCheckpoint(t *testing.T) {
	data := make([]byte, 128)
	copy(data, []byte("cached"))
	server, _ := testserver.NewRangeServer(data)
	defer server.Close()
	manager, index, first := hydratedSession(t, data[:6], int64(len(data)), []model.ByteRange{{Start: 0, End: 6}})
	defer index.Close()
	defer manager.Close()
	manager.mu.Lock()
	manager.client = &http.Client{
		Transport: &rewriteTransport{target: mustParseURL(t, server.URL), base: http.DefaultTransport},
	}
	manager.mu.Unlock()

	blocking := &blockingSessionIndex{Index: index, started: make(chan struct{}), release: make(chan struct{})}
	first.index = blocking
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- manager.Cancel(first.ID()) }()
	<-blocking.started

	createResult := make(chan struct {
		session *Session
		err     error
	}, 1)
	go func() {
		session, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{
			Track: first.task().Track, RemoteURL: testRemoteURL,
		})
		createResult <- struct {
			session *Session
			err     error
		}{session: session, err: err}
	}()

	select {
	case result := <-createResult:
		close(blocking.release)
		t.Fatalf("replacement returned before old shutdown checkpoint completed: session=%p err=%v", result.session, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(blocking.release)
	if err := <-cancelResult; err != nil {
		t.Fatal(err)
	}
	result := <-createResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.session == nil || result.session.ID() == first.ID() {
		t.Fatal("shutdown did not hand the key to a fresh session")
	}
	response := serveRange(t, result.session, "bytes=0-")
	if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), data) {
		t.Fatal("replacement session was not usable after shutdown handoff")
	}
}

func TestBlockedCancellationDoesNotBlockUnrelatedManagerWork(t *testing.T) {
	manager, index, session := hydratedSession(t, []byte("cached"), 128, []model.ByteRange{{Start: 0, End: 6}})
	defer index.Close()
	defer manager.Close()
	server, _ := testserver.NewRangeServer(bytes.Repeat([]byte{0x42}, 128))
	defer server.Close()
	manager.mu.Lock()
	manager.client = &http.Client{
		Transport: &rewriteTransport{target: mustParseURL(t, server.URL), base: http.DefaultTransport},
	}
	manager.mu.Unlock()
	blocking := &blockingSessionIndex{Index: index, started: make(chan struct{}), release: make(chan struct{})}
	session.index = blocking
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	defer release()
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- manager.Cancel(session.ID()) }()
	<-blocking.started

	tasksDone := make(chan struct{}, 1)
	go func() {
		_ = manager.Tasks()
		tasksDone <- struct{}{}
	}()
	createDone := make(chan error, 1)
	go func() {
		_, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{
			Track: model.TrackInfo{
				Hash: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", Quality: "320", Effect: "none", Extension: "mp3",
			},
			RemoteURL: testRemoteURL,
		})
		createDone <- err
	}()

	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	tasksReturned := false
	createReturned := false
	for !tasksReturned || !createReturned {
		select {
		case <-tasksDone:
			tasksReturned = true
		case err := <-createDone:
			if err != nil {
				release()
				t.Fatal(err)
			}
			createReturned = true
		case <-deadline.C:
			release()
			if !tasksReturned {
				<-tasksDone
			}
			if !createReturned {
				if err := <-createDone; err != nil {
					t.Fatal(err)
				}
			}
			if err := <-cancelResult; err != nil {
				t.Fatal(err)
			}
			t.Fatalf("blocked cancellation held global manager lock: tasks returned=%t create returned=%t", tasksReturned, createReturned)
		}
	}
	release()
	if err := <-cancelResult; err != nil {
		t.Fatal(err)
	}
}

func TestRedirectCallbackCanReenterManagerDuringBlockedCancellation(t *testing.T) {
	manager, index, session := hydratedSession(t, []byte("cached"), 128, []model.ByteRange{{Start: 0, End: 6}})
	defer index.Close()
	defer manager.Close()
	blocking := &blockingSessionIndex{Index: index, started: make(chan struct{}), release: make(chan struct{})}
	session.index = blocking
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	defer release()
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- manager.Cancel(session.ID()) }()
	<-blocking.started

	client := secureHTTPClient(&http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		_ = manager.Tasks()
		return nil
	}})
	redirectURL := mustParseURL(t, "https://cdn.example.test/song")
	viaURL := mustParseURL(t, "https://media.example.test/song")
	redirectDone := make(chan error, 1)
	go func() {
		redirectDone <- client.CheckRedirect(&http.Request{
			URL: redirectURL, Header: make(http.Header),
		}, []*http.Request{{URL: viaURL}})
	}()
	select {
	case err := <-redirectDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		release()
		<-redirectDone
		if err := <-cancelResult; err != nil {
			t.Fatal(err)
		}
		t.Fatal("inherited redirect callback deadlocked reentering manager during cancellation")
	}
	release()
	if err := <-cancelResult; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCancelAndCloseShareOneCompletedShutdown(t *testing.T) {
	manager, index, session := hydratedSession(t, []byte("cached"), 128, []model.ByteRange{{Start: 0, End: 6}})
	defer index.Close()
	defer manager.Close()
	blocking := &blockingSessionIndex{Index: index, started: make(chan struct{}), release: make(chan struct{})}
	session.index = blocking
	const callers = 20
	errorsSeen := make(chan error, callers)
	returned := make(chan struct{}, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			defer func() { returned <- struct{}{} }()
			if index%2 == 0 {
				errorsSeen <- manager.Cancel(session.ID())
			} else {
				errorsSeen <- session.close(true)
			}
		}(index)
	}
	<-blocking.started
	select {
	case <-returned:
		t.Fatal("shutdown caller returned before forced checkpoint completed")
	case <-time.After(40 * time.Millisecond):
	}
	close(blocking.release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if session.PlaybackToken() != "" {
		t.Fatal("shutdown did not invalidate playback token")
	}
	entry, ok, _ := index.Lookup(session.Key())
	if !ok || entry.Complete || entry.Size == 0 {
		t.Fatalf("shutdown checkpoint = %#v, found = %t", entry, ok)
	}
}

func TestCanceledTaskHistoryIsBounded(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	manager := NewManager(index, nil)
	defer manager.Close()
	track := model.TrackInfo{Hash: "abababababababababababababababab", Quality: "320", Effect: "none", Extension: "mp3"}
	key, track, err := normalizeTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.Entry{Track: track, RelativePath: partialPath(key), CreatedAt: time.Now().UTC()}
	for count := 0; count < 140; count++ {
		session, err := newSession(manager, key, entry, "", model.TaskNeedsURL)
		if err != nil {
			t.Fatal(err)
		}
		session.mu.Lock()
		session.state = model.TaskCanceled
		session.stopped = true
		session.mu.Unlock()
		session.finish()
		manager.mu.Lock()
		manager.sessions[key] = session
		manager.archiveCanceledLocked(key, session)
		manager.mu.Unlock()
	}
	if got := len(manager.Tasks()); got != 128 {
		t.Fatalf("retained canceled tasks = %d, want 128", got)
	}
}

func TestCheckpointCadenceUsesByteAndTimeThresholds(t *testing.T) {
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	clock := time.Unix(100, 0)
	options := defaultSessionOptions()
	options.checkpointBytes = 128
	options.checkpointInterval = time.Second
	options.now = func() time.Time { return clock }
	manager := newManagerWithOptions(index, nil, options)
	track := model.TrackInfo{Hash: "ffffffffffffffffffffffffffffffff", Quality: "320", Effect: "none", Extension: "mp3"}
	key, normalized, err := normalizeTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.UTC()
	session, err := newSession(manager, key, cache.Entry{
		Track: normalized, RelativePath: partialPath(key), TotalBytes: 1024, CreatedAt: now, LastAccessedAt: now,
	}, "", model.TaskNeedsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.close(false) })
	counting := &countingSessionIndex{Index: index}
	session.index = counting
	file, err := counting.OpenFile(partialPath(key), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	chunk := bytes.Repeat([]byte{0x4d}, 64)
	if _, err := session.writeChunk(file, 0, chunk); err != nil {
		t.Fatal(err)
	}
	if counting.partialUpserts.Load() != 0 {
		t.Fatal("first sub-threshold write persisted the full index")
	}
	if _, err := session.writeChunk(file, 64, chunk); err != nil {
		t.Fatal(err)
	}
	if counting.partialUpserts.Load() != 1 {
		t.Fatalf("byte threshold checkpoints = %d, want 1", counting.partialUpserts.Load())
	}
	if _, err := session.writeChunk(file, 128, chunk); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	if _, err := session.writeChunk(file, 192, chunk); err != nil {
		t.Fatal(err)
	}
	if counting.partialUpserts.Load() != 2 {
		t.Fatalf("time threshold checkpoints = %d, want 2", counting.partialUpserts.Load())
	}
}

func TestHydratedCompletePartFinalizesWithoutURL(t *testing.T) {
	data := bytes.Repeat([]byte("complete-part"), 64)
	manager, index, _ := hydratedSession(t, data, int64(len(data)), []model.ByteRange{{Start: 0, End: int64(len(data))}})
	defer index.Close()
	defer manager.Close()
	key, _ := model.CacheKey("cccccccccccccccccccccccccccccccc", "320", "none")
	waitFor(t, time.Second, func() bool {
		entry, ok, _ := index.Lookup(key)
		return ok && entry.Complete
	}, "crash-complete part finalization")
}

func TestReaderStartingDuringFinalRenameUsesTransitionPath(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	blocking := &blockingCompleteSessionIndex{
		Index: index, started: make(chan struct{}), release: make(chan struct{}),
	}
	session.index = blocking
	session.start()
	<-blocking.started

	responseResult := make(chan *httptest.ResponseRecorder, 1)
	go func() { responseResult <- serveRange(t, session, "bytes=0-") }()
	select {
	case response := <-responseResult:
		if response.Code != http.StatusPartialContent {
			close(blocking.release)
			t.Fatalf("status during finalization = %d, want 206", response.Code)
		}
		if response.Body.Len() == 0 {
			close(blocking.release)
			t.Fatal("reader received no bytes from renamed cache file")
		}
	case <-time.After(time.Second):
		close(blocking.release)
		t.Fatal("reader remained blocked on finalization transition")
	}
	close(blocking.release)
	waitFor(t, time.Second, func() bool { return session.task().State == model.TaskCompleted }, "delayed finalization")
}

func TestCancelDuringFinalizationRemainsCanceled(t *testing.T) {
	for _, test := range []struct {
		name       string
		persistErr error
	}{
		{name: "persist succeeds"},
		{name: "persist fails", persistErr: errors.New("complete persist")},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, index := fullPartialSession(t)
			defer index.Close()
			blocking := &blockingCompleteSessionIndex{
				Index: index, started: make(chan struct{}), release: make(chan struct{}), persistErr: test.persistErr,
			}
			session.index = blocking
			session.start()
			<-blocking.started
			cancelResult := make(chan error, 1)
			go func() { cancelResult <- session.cancelTask() }()
			waitFor(t, time.Second, func() bool { return session.task().State == model.TaskCanceled }, "cancel during finalization")
			close(blocking.release)
			if err := <-cancelResult; err != nil {
				t.Fatal(err)
			}
			if state := session.task().State; state != model.TaskCanceled {
				t.Fatalf("state after finalization race = %s, want canceled", state)
			}
		})
	}
}

func TestCompletionWinningBeforeCancelClaimRemainsActive(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	manager := session.manager
	defer manager.Close()
	manager.mu.Lock()
	manager.sessions[session.Key()] = session
	manager.mu.Unlock()
	blocking := &blockingCompleteSessionIndex{
		Index: index, started: make(chan struct{}), release: make(chan struct{}),
	}
	session.index = blocking
	claimReached := make(chan struct{})
	claimRelease := make(chan struct{})
	var claimReachedOnce sync.Once
	session.options.cancelClaimBarrier = func() {
		claimReachedOnce.Do(func() { close(claimReached) })
		<-claimRelease
	}
	var releaseFinalizeOnce sync.Once
	releaseFinalize := func() { releaseFinalizeOnce.Do(func() { close(blocking.release) }) }
	defer releaseFinalize()
	var releaseClaimOnce sync.Once
	releaseClaim := func() { releaseClaimOnce.Do(func() { close(claimRelease) }) }
	defer releaseClaim()

	session.start()
	<-blocking.started
	token := session.PlaybackToken()
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- manager.Cancel(session.ID()) }()
	<-claimReached
	releaseFinalize()
	waitFor(t, time.Second, func() bool { return session.task().State == model.TaskCompleted }, "completion before cancel claim")
	releaseClaim()
	if err := <-cancelResult; err == nil {
		t.Fatal("cancel succeeded after completion won the state transition")
	}
	manager.mu.Lock()
	owner := manager.sessions[session.Key()]
	_, archived := manager.history[session.ID()]
	manager.mu.Unlock()
	if owner != session || archived {
		t.Fatalf("completed session ownership changed after rejected cancel: owner=%p archived=%t", owner, archived)
	}
	if session.PlaybackToken() != token {
		t.Fatal("rejected cancel invalidated completed playback token")
	}
	if found, ok := manager.Session(session.ID(), token); !ok || found != session {
		t.Fatal("genuinely completed session was removed after rejected cancel")
	}
}

func TestParseContentRangeRejectsSignedAndWhitespaceNumbers(t *testing.T) {
	for _, header := range []string{
		"bytes +0-9/10", "bytes 0-+9/10", "bytes 0-9/+10",
		"bytes  0-9/10", "bytes 0 -9/10", "bytes 0-9/ 10", "bytes \u0660-9/10",
	} {
		if _, _, err := parseContentRange(header); err == nil {
			t.Fatalf("accepted malformed Content-Range %q", header)
		}
	}
}

func TestSecureClientAppliesFinalPolicyAfterInheritedRedirect(t *testing.T) {
	via := []*http.Request{{URL: mustParseURL(t, "https://media.example.test/song?signature=secret")}}
	t.Run("rejects callback-mutated unsafe URL", func(t *testing.T) {
		client := secureHTTPClient(&http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			request.URL = mustParseURL(t, "http://127.0.0.1/private")
			return nil
		}})
		redirect := &http.Request{URL: mustParseURL(t, "https://cdn.example.test/song"), Header: make(http.Header)}
		if err := client.CheckRedirect(redirect, via); err == nil {
			t.Fatal("callback-mutated private redirect target was accepted")
		}
	})
	t.Run("strips callback-reinjected cross-host secrets", func(t *testing.T) {
		client := secureHTTPClient(&http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			request.URL = mustParseURL(t, "https://user:password@final.example.test/song")
			request.Header.Set("Authorization", "Bearer reinjected")
			request.Header.Set("Proxy-Authorization", "Basic reinjected")
			request.Header.Set("Cookie", "session=reinjected")
			request.Header.Set("Referer", "https://media.example.test/song?signature=secret")
			return nil
		}})
		redirect := &http.Request{URL: mustParseURL(t, "https://cdn.example.test/song"), Header: make(http.Header)}
		if err := client.CheckRedirect(redirect, via); err != nil {
			t.Fatal(err)
		}
		if redirect.Header.Get("Authorization") != "" || redirect.Header.Get("Proxy-Authorization") != "" ||
			redirect.Header.Get("Cookie") != "" || redirect.Header.Get("Referer") != "" || redirect.URL.User != nil {
			t.Fatalf("inherited redirect callback bypassed final secret stripping: URL=%s headers=%#v", redirect.URL, redirect.Header)
		}
	})
}

func TestShortWriteCheckpointsOnlyWrittenBytesAndFails(t *testing.T) {
	data := bytes.Repeat([]byte{0x3c}, 1024)
	server, _ := testserver.NewRangeServer(data)
	defer server.Close()
	options := fastFailureOptions()
	var once sync.Once
	options.writeAt = func(file *os.File, value []byte, offset int64) (int, error) {
		written := 0
		var writeErr error
		once.Do(func() {
			written, writeErr = file.WriteAt(value[:17], offset)
		})
		if written != 0 || writeErr != nil {
			return written, writeErr
		}
		return file.WriteAt(value, offset)
	}
	manager, index := newReviewManager(t, server.URL, options)
	defer index.Close()
	defer manager.Close()
	session := createReviewSession(t, manager)
	waitFor(t, time.Second, func() bool { return session.task().State == model.TaskFailed }, "short write failure")
	entry, ok, _ := index.Lookup(session.Key())
	if !ok || len(entry.Ranges) != 1 || entry.Ranges[0] != (model.ByteRange{Start: 0, End: 17}) {
		t.Fatalf("short-write checkpoint = %#v, found = %t", entry.Ranges, ok)
	}
}

func TestFinalizeRepairsCompleteEntryWhenPersistAndRollbackInitiallyFail(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	persistFailure := errors.New("persist complete")
	rollbackFailure := errors.New("rollback rename")
	faults := &faultingSessionIndex{
		Index: index, completeFailures: 1, rollbackFailures: 1,
		persistFailure: persistFailure, rollbackFailure: rollbackFailure,
	}
	session.index = faults
	if err := session.finalize(); err != nil {
		t.Fatalf("finalize did not repair a rooted final file: %v", err)
	}
	entry, ok, _ := index.Lookup(session.Key())
	if !ok || !entry.Complete {
		t.Fatalf("repaired entry = %#v, found = %t", entry, ok)
	}
}

func TestFinalizeSurfacesJoinedErrorsWhenReconciliationFails(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	persistFailure := errors.New("persist complete")
	rollbackFailure := errors.New("rollback rename")
	faults := &faultingSessionIndex{
		Index: index, completeFailures: 2, rollbackFailures: 1,
		persistFailure: persistFailure, rollbackFailure: rollbackFailure,
	}
	session.index = faults
	err := session.finalize()
	if !errors.Is(err, persistFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("finalize error = %v, want joined persist and rollback failures", err)
	}
}

func TestFinalizeJoinsUnexpectedRootedStatFailure(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	persistFailure := errors.New("persist complete")
	rollbackFailure := errors.New("rollback rename")
	statFailure := errors.New("stat final")
	faults := &faultingSessionIndex{
		Index: index, completeFailures: 1, rollbackFailures: 1, rollbackAfterRename: true,
		persistFailure: persistFailure, rollbackFailure: rollbackFailure,
		finalStatFailureOnCall: 2, statFailure: statFailure,
	}
	session.index = faults
	err := session.finalize()
	if !errors.Is(err, persistFailure) || !errors.Is(err, rollbackFailure) || !errors.Is(err, statFailure) {
		t.Fatalf("finalize error = %v, want joined persist, rollback, and stat failures", err)
	}
	entry, ok, _ := index.Lookup(session.Key())
	if !ok || entry.Complete {
		t.Fatalf("partial reconciliation entry = %#v, found = %t", entry, ok)
	}
}

func TestFinalizeJoinsRootedCleanupFailure(t *testing.T) {
	session, index := fullPartialSession(t)
	defer index.Close()
	persistFailure := errors.New("persist complete")
	rollbackFailure := errors.New("rollback rename")
	removeFailure := errors.New("remove final")
	faults := &faultingSessionIndex{
		Index: index, completeFailures: 1, rollbackFailures: 1, rollbackAfterRename: true,
		persistFailure: persistFailure, rollbackFailure: rollbackFailure,
		removeFailures: 1, removeFailure: removeFailure,
	}
	session.index = faults
	err := session.finalize()
	if !errors.Is(err, persistFailure) || !errors.Is(err, rollbackFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("finalize error = %v, want joined persist, rollback, and cleanup failures", err)
	}
	entry, ok, _ := index.Lookup(session.Key())
	if !ok || entry.Complete {
		t.Fatalf("partial reconciliation entry = %#v, found = %t", entry, ok)
	}
}

type hostMapTransport struct {
	targets map[string]*url.URL
	base    http.RoundTripper
}

func (transport *hostMapTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	target := transport.targets[request.URL.Hostname()]
	if target == nil {
		return nil, fmt.Errorf("no test target for %q", request.URL.Hostname())
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme = target.Scheme
	clone.URL.Host = target.Host
	clone.Host = target.Host
	return transport.base.RoundTrip(clone)
}

func fastFailureOptions() sessionOptions {
	options := defaultSessionOptions()
	options.responseHeaderTimeout = 100 * time.Millisecond
	options.bodyIdleTimeout = 100 * time.Millisecond
	options.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	options.jitter = func(delay time.Duration) time.Duration { return delay }
	return options
}

func newReviewManager(t *testing.T, target string, options sessionOptions) (*Manager, *cache.Index) {
	t.Helper()
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &rewriteTransport{target: mustParseURL(t, target), base: http.DefaultTransport}
	manager := newManagerWithOptions(index, nil, options)
	manager.client = &http.Client{Transport: transport}
	return manager, index
}

func createReviewSession(t *testing.T, manager *Manager) *Session {
	t.Helper()
	track := model.TrackInfo{Hash: "dddddddddddddddddddddddddddddddd", Quality: "320", Effect: "none", Extension: "mp3"}
	session, err := manager.CreateOrReuse(context.Background(), model.CreateSessionRequest{Track: track, RemoteURL: testRemoteURL})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func hydratedSession(t *testing.T, data []byte, total int64, ranges []model.ByteRange) (*Manager, *cache.Index, *Session) {
	t.Helper()
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	track := model.TrackInfo{Hash: "cccccccccccccccccccccccccccccccc", Quality: "320", Effect: "none", Extension: "mp3"}
	key, _ := model.CacheKey(track.Hash, track.Quality, track.Effect)
	file, err := index.OpenFile(partialPath(key), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	now := time.Now().UTC()
	if err := index.Upsert(key, cache.Entry{
		Track: track, RelativePath: partialPath(key), Size: int64(len(data)), TotalBytes: total,
		Ranges: ranges, CreatedAt: now, LastAccessedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(index, nil)
	session := manager.sessions[key]
	if session == nil {
		t.Fatal("incomplete entry was not hydrated")
	}
	return manager, index, session
}

func fullPartialSession(t *testing.T) (*Session, *cache.Index) {
	t.Helper()
	index, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(index, nil)
	track := model.TrackInfo{Hash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Quality: "320", Effect: "none", Extension: "mp3"}
	key, normalized, err := normalizeTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("finalize"), 128)
	now := time.Now().UTC()
	entry := cache.Entry{
		Track: normalized, RelativePath: partialPath(key), Size: int64(len(data)), TotalBytes: int64(len(data)),
		Ranges: []model.ByteRange{{Start: 0, End: int64(len(data))}}, CreatedAt: now, LastAccessedAt: now,
	}
	file, err := index.OpenFile(entry.RelativePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := index.Upsert(key, entry); err != nil {
		t.Fatal(err)
	}
	session, err := newSession(manager, key, entry, "", model.TaskNeedsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.close(false) })
	return session, index
}

type faultingSessionIndex struct {
	*cache.Index
	mu                     sync.Mutex
	completeFailures       int
	rollbackFailures       int
	rollbackAfterRename    bool
	removeFailures         int
	persistFailure         error
	rollbackFailure        error
	removeFailure          error
	finalStatCalls         int
	finalStatFailureOnCall int
	statFailure            error
}

type blockingSessionIndex struct {
	*cache.Index
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

type blockingCompleteSessionIndex struct {
	*cache.Index
	once       sync.Once
	started    chan struct{}
	release    chan struct{}
	persistErr error
}

func (index *blockingCompleteSessionIndex) Upsert(key string, entry cache.Entry) error {
	if entry.Complete {
		index.once.Do(func() {
			close(index.started)
			<-index.release
		})
		if index.persistErr != nil {
			return index.persistErr
		}
	}
	return index.Index.Upsert(key, entry)
}

func (index *blockingSessionIndex) Upsert(key string, entry cache.Entry) error {
	index.once.Do(func() {
		close(index.started)
		<-index.release
	})
	return index.Index.Upsert(key, entry)
}

type countingSessionIndex struct {
	*cache.Index
	partialUpserts atomic.Int64
}

func (index *countingSessionIndex) Upsert(key string, entry cache.Entry) error {
	if !entry.Complete {
		index.partialUpserts.Add(1)
	}
	return index.Index.Upsert(key, entry)
}

func (index *faultingSessionIndex) Upsert(key string, entry cache.Entry) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	if entry.Complete && index.completeFailures > 0 {
		index.completeFailures--
		return index.persistFailure
	}
	return index.Index.Upsert(key, entry)
}

func (index *faultingSessionIndex) RenameFile(oldPath, newPath string) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	if strings.HasPrefix(oldPath, "audio/") && index.rollbackFailures > 0 {
		index.rollbackFailures--
		if index.rollbackAfterRename {
			if err := index.Index.RenameFile(oldPath, newPath); err != nil {
				return errors.Join(index.rollbackFailure, err)
			}
		}
		return index.rollbackFailure
	}
	return index.Index.RenameFile(oldPath, newPath)
}

func (index *faultingSessionIndex) StatFile(path string) (os.FileInfo, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	if strings.HasPrefix(path, "audio/") {
		index.finalStatCalls++
		if index.finalStatCalls == index.finalStatFailureOnCall {
			return nil, index.statFailure
		}
	}
	return index.Index.StatFile(path)
}

func (index *faultingSessionIndex) RemoveFile(path string) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	if strings.HasPrefix(path, "audio/") && index.removeFailures > 0 {
		index.removeFailures--
		return index.removeFailure
	}
	return index.Index.RemoveFile(path)
}
