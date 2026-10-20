//go:build windows

package server

import (
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	revealShell              = syscall.NewLazyDLL("shell32.dll")
	revealOLE                = syscall.NewLazyDLL("ole32.dll")
	parseDisplayName         = revealShell.NewProc("SHParseDisplayName")
	openFolderAndSelectItems = revealShell.NewProc("SHOpenFolderAndSelectItems")
	findLastID               = revealShell.NewProc("ILFindLastID")
	initializeCOM            = revealOLE.NewProc("CoInitializeEx")
	uninitializeCOM          = revealOLE.NewProc("CoUninitialize")
	freeShellMemory          = revealOLE.NewProc("CoTaskMemFree")
)

// Use shell item IDs rather than Explorer's command-line parser. This also
// returns an HRESULT when the shell cannot open the requested location.
func revealFile(kind, target string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result, _, _ := initializeCOM.Call(0, 2) // COINIT_APARTMENTTHREADED
	if err := shellResult("CoInitializeEx", result); err != nil {
		return err
	}
	defer uninitializeCOM.Call()

	folder := target
	if kind == "select" {
		folder = filepath.Dir(target)
	}
	folderID, err := shellItemID(folder)
	if err != nil {
		return err
	}
	defer freeShellMemory.Call(folderID)

	var count uintptr
	var child uintptr
	var fileID uintptr
	if kind == "select" {
		fileID, err = shellItemID(target)
		if err != nil {
			return err
		}
		defer freeShellMemory.Call(fileID)
		child, _, _ = findLastID.Call(fileID)
		if child == 0 {
			return fmt.Errorf("cannot locate shell item for %q", target)
		}
		count = 1
	}
	var children *uintptr
	if count != 0 {
		children = &child
	}
	result, _, _ = openFolderAndSelectItems.Call(folderID, count, uintptr(unsafe.Pointer(children)), 0)
	runtime.KeepAlive(child)
	return shellResult("SHOpenFolderAndSelectItems", result)
}

func shellItemID(path string) (uintptr, error) {
	value, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var item uintptr
	result, _, _ := parseDisplayName.Call(uintptr(unsafe.Pointer(value)), 0, uintptr(unsafe.Pointer(&item)), 0, 0)
	runtime.KeepAlive(value)
	if err := shellResult("SHParseDisplayName", result); err != nil {
		return 0, err
	}
	if item == 0 {
		return 0, fmt.Errorf("shell returned an empty item for %q", path)
	}
	return item, nil
}

func shellResult(operation string, result uintptr) error {
	if int32(result) < 0 {
		return fmt.Errorf("%s failed: HRESULT 0x%08x", operation, uint32(result))
	}
	return nil
}
