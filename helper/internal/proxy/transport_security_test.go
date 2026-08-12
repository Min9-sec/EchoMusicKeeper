package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (function testRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSecureDialRejectsNonPublicAndMixedDNSAnswers(t *testing.T) {
	cases := []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "loopback", addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		{name: "link local", addresses: []netip.Addr{netip.MustParseAddr("169.254.1.1")}},
		{name: "unspecified", addresses: []netip.Addr{netip.MustParseAddr("::")}},
		{name: "multicast", addresses: []netip.Addr{netip.MustParseAddr("ff02::1")}},
		{name: "NAT64 private mapping", addresses: []netip.Addr{netip.MustParseAddr("64:ff9b::a00:1")}},
		{name: "mixed", addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.168.1.1")}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var dialed atomic.Int32
			client := secureHTTPClientWithNetwork(nil,
				func(context.Context, string, string) ([]netip.Addr, error) { return test.addresses, nil },
				func(context.Context, string, string) (net.Conn, error) {
					dialed.Add(1)
					return nil, errors.New("unexpected dial")
				},
			)
			transport := client.Transport.(*http.Transport)
			if _, err := transport.DialContext(context.Background(), "tcp", "media.example.test:443"); err == nil {
				t.Fatal("non-public DNS answer was accepted")
			}
			if dialed.Load() != 0 {
				t.Fatalf("base dialer called %d times", dialed.Load())
			}
		})
	}
}

func TestSecureHTTPClientDisablesProvidedNetworkHooks(t *testing.T) {
	var dialCalls atomic.Int32
	var dialTLSCalls atomic.Int32
	var proxyCalls atomic.Int32
	provided := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			proxyCalls.Add(1)
			return nil, nil
		},
		Dial: func(string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("provided Dial must not run")
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("provided DialContext must not run")
		},
		DialTLS: func(string, string) (net.Conn, error) {
			dialTLSCalls.Add(1)
			return nil, errors.New("provided DialTLS must not run")
		},
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			dialTLSCalls.Add(1)
			return nil, errors.New("provided DialTLSContext must not run")
		},
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{
			"h2": func(string, *tls.Conn) http.RoundTripper {
				return testRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("provided TLSNextProto must not run")
				})
			},
		},
	}}
	client := secureHTTPClient(provided)
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.Dial != nil || transport.DialTLS != nil || transport.DialTLSContext != nil || transport.TLSNextProto != nil {
		t.Fatalf("unsafe transport hooks survived: %+v", transport)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.DialContext(ctx, "tcp", "8.8.8.8:443"); err == nil {
		t.Fatal("canceled internal dial unexpectedly succeeded")
	}
	if dialCalls.Load() != 0 || dialTLSCalls.Load() != 0 || proxyCalls.Load() != 0 {
		t.Fatalf("provided hook calls: dial=%d dialTLS=%d proxy=%d", dialCalls.Load(), dialTLSCalls.Load(), proxyCalls.Load())
	}
}

func TestSecureHTTPClientRejectsInitialPrivateDNSBeforeDial(t *testing.T) {
	var dialed atomic.Int32
	client := secureHTTPClientWithNetwork(nil,
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return nil, errors.New("private target must not be dialed")
		},
	)
	response, err := client.Get("http://private.example.test/song")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("initial private DNS error = %v", err)
	}
	if dialed.Load() != 0 {
		t.Fatalf("private target dialed %d times", dialed.Load())
	}
}

func TestSecureHTTPClientDoesNotUseConfiguredProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var proxyCalls atomic.Int32
	provided := &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		proxyCalls.Add(1)
		return url.Parse("http://127.0.0.1:1")
	}}}
	target := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	client := secureHTTPClientWithNetwork(provided,
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:80" {
				return nil, fmt.Errorf("dial address = %q", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
	)
	response, err := client.Get("http://media.example.test/song")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || proxyCalls.Load() != 0 {
		t.Fatalf("status = %d, proxy calls = %d", response.StatusCode, proxyCalls.Load())
	}
}

func TestSecureDialPinsValidatedAddressAndRevalidatesEachConnection(t *testing.T) {
	var lookups atomic.Int32
	var dialed []string
	client := secureHTTPClientWithNetwork(nil,
		func(context.Context, string, string) ([]netip.Addr, error) {
			if lookups.Add(1) == 1 {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("stop after address capture")
		},
	)
	transport := client.Transport.(*http.Transport)
	if _, err := transport.DialContext(context.Background(), "tcp", "rebind.example.test:8443"); err == nil {
		t.Fatal("captured test dial unexpectedly succeeded")
	}
	if len(dialed) != 1 || dialed[0] != "8.8.8.8:8443" {
		t.Fatalf("dialed addresses = %v", dialed)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "rebind.example.test:8443"); err == nil {
		t.Fatal("private rebound answer was accepted")
	}
	if len(dialed) != 1 || lookups.Load() != 2 {
		t.Fatalf("lookups = %d, dialed addresses = %v", lookups.Load(), dialed)
	}
}

func TestSecureHTTPClientPreservesOriginalTLSHostname(t *testing.T) {
	serverName := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		serverName <- request.TLS.ServerName
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := server.Certificate().VerifyHostname("example.com"); err != nil {
		t.Fatalf("test certificate does not cover example.com: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	target := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	provided := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	client := secureHTTPClientWithNetwork(provided,
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				return nil, fmt.Errorf("dial address = %q", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
	)
	response, err := client.Get("https://example.com/song?token=ephemeral")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := <-serverName; got != "example.com" {
		t.Fatalf("TLS SNI = %q, want example.com", got)
	}
}

func TestSecureHTTPClientRejectsCertificateForDifferentHostname(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	if err := server.Certificate().VerifyHostname("wrong.example"); err == nil {
		t.Fatal("test certificate unexpectedly covers wrong.example")
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	target := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	provided := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10,
	}}}
	client := secureHTTPClientWithNetwork(provided,
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				return nil, fmt.Errorf("dial address = %q", address)
			}
			return dialer.DialContext(ctx, network, target)
		},
	)
	response, err := client.Get("https://wrong.example/song")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("hostname mismatch error = %v", err)
	}
}

func TestRedirectHostnameIsResolvedBeforeConnection(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "http://private.example.test/metadata", http.StatusFound)
	}))
	defer source.Close()
	target := source.Listener.Addr().String()
	dialer := &net.Dialer{}
	var dialed atomic.Int32
	client := secureHTTPClientWithNetwork(nil,
		func(_ context.Context, _, host string) ([]netip.Addr, error) {
			if host == "source.example.test" {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialed.Add(1)
			return dialer.DialContext(ctx, network, target)
		},
	)
	response, err := client.Get("http://source.example.test/song?token=ephemeral")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("redirect error = %v", err)
	}
	if dialed.Load() != 1 {
		t.Fatalf("actual connections = %d, want one public source connection", dialed.Load())
	}
}

func TestSecureHTTPClientRejectsUnpinnableCustomTransport(t *testing.T) {
	var calls atomic.Int32
	client := secureHTTPClient(&http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not run")
	})})
	request, err := http.NewRequest(http.MethodGet, "https://example.com/song", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "custom transport") {
		t.Fatalf("custom transport error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("custom transport called %d times", calls.Load())
	}
}
