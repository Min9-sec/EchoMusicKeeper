//go:build windows

package security

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	winFileReadAttributes       = 0x00000080
	winFileDispositionInfoClass = 4
)

var (
	kernel32Delete                 = syscall.NewLazyDLL("kernel32.dll")
	reOpenFileProcedure            = kernel32Delete.NewProc("ReOpenFile")
	setFileInformationByHandleProc = kernel32Delete.NewProc("SetFileInformationByHandle")
)

type winFileDispositionInfo struct {
	DeleteFile bool
}

func CanRemoveOpenedDirectory() bool { return true }

func removeOpenedDirectory(root *os.Root, afterCheck func()) (removed bool, resultErr error) {
	opened, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("open directory disposition source: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, opened.Close())
	}()

	reopened, _, callErr := reOpenFileProcedure.Call(
		opened.Fd(),
		uintptr(ntDelete|ntSynchronize|winFileReadAttributes),
		uintptr(ntFileShareRead|ntFileShareWrite|ntFileShareDelete),
		uintptr(syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT),
	)
	runtime.KeepAlive(opened)
	deleteHandle := syscall.Handle(reopened)
	if deleteHandle == syscall.InvalidHandle {
		return false, windowsDeleteCallError("ReOpenFile directory for disposition", callErr)
	}
	defer func() {
		resultErr = errors.Join(resultErr, syscall.CloseHandle(deleteHandle))
	}()

	if afterCheck != nil {
		afterCheck()
	}
	disposition := winFileDispositionInfo{DeleteFile: true}
	succeeded, _, callErr := setFileInformationByHandleProc.Call(
		uintptr(deleteHandle),
		uintptr(winFileDispositionInfoClass),
		uintptr(unsafe.Pointer(&disposition)),
		unsafe.Sizeof(disposition),
	)
	runtime.KeepAlive(disposition)
	if succeeded == 0 {
		return false, windowsDeleteCallError("SetFileInformationByHandle directory disposition", callErr)
	}
	return true, nil
}

func windowsDeleteCallError(operation string, err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s failed without a Windows error code", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
