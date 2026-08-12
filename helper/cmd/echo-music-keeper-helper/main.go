package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/knownfolder"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/proxy"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/server"
)

var version = "dev"

const shutdownTimeout = 5 * time.Second

type options struct {
	port            int
	token           string
	pluginRoot      string
	cacheRoot       string
	downloadRoot    string
	cacheLimitBytes int64
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("cache helper failed: %v", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	configuration, err := parseFlags(arguments)
	if err != nil {
		return err
	}
	defaultMusicPath := configuration.downloadRoot
	if configuration.downloadRoot == "" {
		defaultMusicPath, err = knownfolder.Music()
		if err != nil {
			return fmt.Errorf("resolve default Music directory: %w", err)
		}
		configuration.downloadRoot = defaultMusicPath
	} else if resolved, resolveErr := knownfolder.Music(); resolveErr == nil {
		defaultMusicPath = resolved
	}
	index, err := cache.Open(configuration.cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache index: %w", err)
	}
	manager := proxy.NewManager(index, nil)
	cleanupManager := true
	defer func() {
		if cleanupManager {
			_ = manager.Close()
		}
	}()
	if err := manager.SetDownloadRoots(configuration.downloadRoot, nil); err != nil {
		return fmt.Errorf("configure download root: %w", err)
	}
	if err := manager.SetDownloadRecords(nil); err != nil {
		return fmt.Errorf("restore download records: %w", err)
	}
	if err := manager.SetLimit(configuration.cacheLimitBytes); err != nil {
		return fmt.Errorf("configure cache limit: %w", err)
	}

	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(configuration.port)))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer listener.Close()
	shutdownRequested := make(chan struct{})
	var shutdownOnce sync.Once
	handler := server.New(server.Options{
		Token: configuration.token, Manager: manager, Version: version,
		DefaultMusicPath: defaultMusicPath, Logger: log.New(os.Stderr, "cache-helper: ", log.LstdFlags),
		Config: model.Config{
			CacheRoot: configuration.cacheRoot, DownloadRoot: configuration.downloadRoot,
			ManagedDownloadRoots: nil, CompletedDownloadPaths: nil, CacheLimitBytes: configuration.cacheLimitBytes,
		},
		Shutdown: func() { shutdownOnce.Do(func() { close(shutdownRequested) }) },
	})
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	cleanupManager = false
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	var serveErr error
	select {
	case <-shutdownRequested:
	case <-interrupts:
	case serveErr = <-serveDone:
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := shutdownServices(shutdownContext, manager.Close, httpServer.Shutdown)
	if serveErr == nil {
		select {
		case serveErr = <-serveDone:
		case <-shutdownContext.Done():
			serveErr = shutdownContext.Err()
		}
	}
	cancel()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, serveErr)
}

func shutdownServices(ctx context.Context, closeManager func() error, shutdownServer func(context.Context) error) error {
	managerDone := make(chan error, 1)
	go func() { managerDone <- closeManager() }()

	var managerErr error
	select {
	case managerErr = <-managerDone:
	case <-ctx.Done():
		startShutdown(ctx, shutdownServer)
		return ctx.Err()
	}

	serverDone := startShutdown(ctx, shutdownServer)
	select {
	case serverErr := <-serverDone:
		return errors.Join(managerErr, serverErr)
	case <-ctx.Done():
		return errors.Join(managerErr, ctx.Err())
	}
}

func startShutdown(ctx context.Context, shutdownServer func(context.Context) error) <-chan error {
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- shutdownServer(ctx)
	}()
	<-started
	return done
}

func parseFlags(arguments []string) (options, error) {
	var result options
	flags := flag.NewFlagSet("cache-helper", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.IntVar(&result.port, "port", 0, "loopback TCP port")
	flags.StringVar(&result.token, "token", "", "control bearer token")
	flags.StringVar(&result.pluginRoot, "plugin-root", "", "absolute plugin root")
	flags.StringVar(&result.cacheRoot, "cache-root", "", "absolute cache root")
	flags.StringVar(&result.downloadRoot, "download-root", "", "absolute download root")
	flags.Int64Var(&result.cacheLimitBytes, "cache-limit-bytes", -1, "cache capacity in bytes")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments")
	}
	if result.port < 1 || result.port > 65535 {
		return options{}, fmt.Errorf("--port must be between 1 and 65535")
	}
	if strings.TrimSpace(result.token) == "" {
		return options{}, fmt.Errorf("--token is required")
	}
	if err := requireAbsolute("--plugin-root", result.pluginRoot); err != nil {
		return options{}, err
	}
	if err := requireAbsolute("--cache-root", result.cacheRoot); err != nil {
		return options{}, err
	}
	if result.downloadRoot != "" {
		if err := requireAbsolute("--download-root", result.downloadRoot); err != nil {
			return options{}, err
		}
	}
	if result.cacheLimitBytes < 0 {
		return options{}, fmt.Errorf("--cache-limit-bytes is required and must not be negative")
	}
	return result, nil
}

func requireAbsolute(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be absolute", name)
	}
	return nil
}
