//go:build !windows

package server

import (
	"fmt"
	"os/exec"
	"runtime"
)

func revealFile(kind, target string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("file manager reveal is only available on Windows and macOS")
	}
	args := []string{target}
	if kind == "select" {
		args = []string{"-R", target}
	}
	command := exec.Command("open", args...)
	if err := command.Start(); err != nil {
		return err
	}
	// Reap the child after launch without blocking the HTTP handler.
	go func() { _ = command.Wait() }()
	return nil
}
