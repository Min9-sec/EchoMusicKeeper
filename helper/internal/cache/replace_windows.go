//go:build windows

package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

type lockedRootPath struct {
	handles []syscall.Handle
}

func lockRootPath(root string) (rootPathLock, error) {
	volume := filepath.VolumeName(root)
	anchor := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(filepath.Clean(root), anchor)
	current := anchor
	locked := &lockedRootPath{}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		pointer, err := syscall.UTF16PtrFromString(current)
		if err != nil {
			_ = locked.Close()
			return nil, err
		}
		handle, err := syscall.CreateFile(
			pointer,
			0,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
			nil,
			syscall.OPEN_EXISTING,
			syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if err != nil {
			_ = locked.Close()
			return nil, fmt.Errorf("lock cache root ancestor %q: %w", current, err)
		}
		var info syscall.ByHandleFileInformation
		if err := syscall.GetFileInformationByHandle(handle, &info); err != nil ||
			info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 ||
			info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			_ = syscall.CloseHandle(handle)
			_ = locked.Close()
			if err != nil {
				return nil, fmt.Errorf("inspect cache root ancestor %q: %w", current, err)
			}
			return nil, fmt.Errorf("cache root ancestor %q is not a real directory", current)
		}
		locked.handles = append(locked.handles, handle)
	}
	return locked, nil
}

func (locked *lockedRootPath) Close() error {
	var closeError error
	for index := len(locked.handles) - 1; index >= 0; index-- {
		closeError = errors.Join(closeError, syscall.CloseHandle(locked.handles[index]))
	}
	locked.handles = nil
	return closeError
}

func replaceFile(_ *os.Root, rootPath, source, destination string) error {
	source = filepath.Join(rootPath, source)
	destination = filepath.Join(rootPath, destination)
	sourcePointer, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(sourcePointer)),
		uintptr(unsafe.Pointer(destinationPointer)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if result == 0 {
		return fmt.Errorf("MoveFileExW: %w", callErr)
	}
	return nil
}

func openRootFile(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flag, perm)
}
