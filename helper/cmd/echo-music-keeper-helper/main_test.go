package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildVersionCanBeInjected(t *testing.T) {
	previous := version
	version = "0.1.3-test"
	t.Cleanup(func() { version = previous })

	if version != "0.1.3-test" {
		t.Fatalf("version = %q", version)
	}
}

func TestParseFlagsRequiresSecurityAndStorageArguments(t *testing.T) {
	absolute := t.TempDir()
	valid := []string{
		"--port", "43123", "--token", strings.Repeat("a", 64),
		"--plugin-root", absolute, "--cache-root", filepath.Join(absolute, "cache"),
		"--cache-limit-bytes", "0",
	}
	configuration, err := parseFlags(valid)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.port != 43123 || configuration.cacheLimitBytes != 0 || configuration.downloadRoot != "" {
		t.Fatalf("configuration = %+v", configuration)
	}

	tests := []struct {
		name string
		args []string
	}{
		{"port", withoutFlag(valid, "--port")},
		{"token", withoutFlag(valid, "--token")},
		{"plugin root", withoutFlag(valid, "--plugin-root")},
		{"cache root", withoutFlag(valid, "--cache-root")},
		{"cache limit", withoutFlag(valid, "--cache-limit-bytes")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseFlags(test.args); err == nil {
				t.Fatal("missing required flag was accepted")
			}
		})
	}
}

func TestParseFlagsRejectsRelativeRoots(t *testing.T) {
	_, err := parseFlags([]string{
		"--port", "43123", "--token", "secret", "--plugin-root", "relative",
		"--cache-root", "/absolute/cache", "--cache-limit-bytes", "1",
	})
	if err == nil || !strings.Contains(err.Error(), "--plugin-root must be absolute") {
		t.Fatalf("error = %v", err)
	}
}

func withoutFlag(arguments []string, target string) []string {
	result := make([]string, 0, len(arguments)-2)
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == target {
			index++
			continue
		}
		result = append(result, arguments[index])
	}
	return result
}

func TestShutdownServicesUsesOneDeadlineWhenManagerCloseStalls(t *testing.T) {
	managerStarted := make(chan struct{})
	managerRelease := make(chan struct{})
	defer close(managerRelease)
	serverCalled := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := shutdownServices(ctx, func() error {
		close(managerStarted)
		<-managerRelease
		return nil
	}, func(context.Context) error {
		serverCalled <- struct{}{}
		return nil
	})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("shutdown elapsed = %s, want a bounded return", elapsed)
	}
	select {
	case <-managerStarted:
	default:
		t.Fatal("manager closure was not initiated")
	}
	select {
	case <-serverCalled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("HTTP shutdown was not attempted at the shared deadline")
	}
}

func TestShutdownServicesDoesNotResetDeadlineForHTTPShutdown(t *testing.T) {
	serverRelease := make(chan struct{})
	defer close(serverRelease)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := shutdownServices(ctx, func() error {
		time.Sleep(35 * time.Millisecond)
		return nil
	}, func(context.Context) error {
		<-serverRelease
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 160*time.Millisecond {
		t.Fatalf("shutdown elapsed = %s, deadline was reset", elapsed)
	}
}
