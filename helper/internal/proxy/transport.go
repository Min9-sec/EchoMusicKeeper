package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/security"
)

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

type rejectedCustomTransport struct{}

func (rejectedCustomTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("custom transport cannot guarantee validated-IP dialing")
}

func secureHTTPClient(provided *http.Client) *http.Client {
	dialer := &net.Dialer{Timeout: connectionTimeout, KeepAlive: 30 * time.Second}
	return secureHTTPClientWithNetwork(provided, net.DefaultResolver.LookupNetIP, dialer.DialContext)
}

func secureHTTPClientWithNetwork(provided *http.Client, lookup lookupNetIPFunc, dial dialContextFunc) *http.Client {
	var client http.Client
	if provided != nil {
		client = *provided
	}
	// Signed source URLs are self-contained. Never attach a caller's cookies.
	client.Jar = nil
	client.Timeout = 0
	secureRedirects(&client)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	switch current := client.Transport.(type) {
	case nil:
	case *http.Transport:
		if current.TLSClientConfig != nil {
			transport.TLSClientConfig = current.TLSClientConfig.Clone()
		}
	default:
		client.Transport = rejectedCustomTransport{}
		return &client
	}
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: connectionTimeout, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport.Proxy = nil
	transport.OnProxyConnectResponse = nil
	transport.DialContext = validatedDialContext(lookup, dial)
	transport.Dial = nil
	transport.DialTLS = nil
	transport.DialTLSContext = nil
	transport.TLSNextProto = nil
	transport.ProxyConnectHeader = nil
	transport.GetProxyConnectHeader = nil
	transport.ResponseHeaderTimeout = connectionTimeout
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.ServerName = ""
		transport.TLSClientConfig.InsecureSkipVerify = false
		if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
	}
	client.Transport = transport
	return &client
}

func validatedDialContext(lookup lookupNetIPFunc, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("invalid upstream dial target")
		}
		addresses, err := resolveDialAddresses(ctx, network, host, lookup)
		if err != nil {
			return nil, err
		}
		for _, candidate := range addresses {
			if !security.IsPublicRemoteIP(candidate) {
				return nil, fmt.Errorf("upstream address for %q is not public", host)
			}
		}
		var failures []error
		for _, candidate := range addresses {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(candidate.Unmap().String(), port))
			if dialErr == nil {
				return connection, nil
			}
			failures = append(failures, dialErr)
		}
		return nil, fmt.Errorf("connect to validated upstream host %q: %w", host, errors.Join(failures...))
	}
}

func resolveDialAddresses(ctx context.Context, network, host string, lookup lookupNetIPFunc) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{literal}, nil
	}
	lookupNetwork := "ip"
	if strings.HasSuffix(network, "4") {
		lookupNetwork = "ip4"
	} else if strings.HasSuffix(network, "6") {
		lookupNetwork = "ip6"
	}
	addresses, err := lookup(ctx, lookupNetwork, host)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve upstream host %q: no addresses", host)
	}
	return addresses, nil
}

func secureRedirects(client *http.Client) {
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		previousHosts := make([]string, 0, len(via))
		for _, previous := range via {
			if previous != nil && previous.URL != nil {
				previousHosts = append(previousHosts, previous.URL.Host)
			}
		}
		if previousRedirect != nil {
			if err := previousRedirect(request, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if request == nil || request.URL == nil {
			return fmt.Errorf("redirect URL is required")
		}
		if _, err := security.ValidateRemoteURL(request.URL.String()); err != nil {
			return err
		}
		for _, previousHost := range previousHosts {
			if !strings.EqualFold(previousHost, request.URL.Host) {
				request.Header.Del("Authorization")
				request.Header.Del("Proxy-Authorization")
				request.Header.Del("Cookie")
				request.Header.Del("Referer")
				request.URL.User = nil
				break
			}
		}
		return nil
	}
}
