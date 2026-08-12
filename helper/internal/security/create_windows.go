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
	ntFileListDirectory   = 0x00000001
	ntFileReadEA          = 0x00000008
	ntFileReadAttributes  = 0x00000080
	ntReadControl         = 0x00020000
	ntFileAttributeNormal = 0x00000080
	ntFileCreate          = 0x00000002
	ntFileDirectoryFile   = 0x00000001
	ntObjectDontReparse   = 0x00001000
	ntStatusNameCollision = 0xC0000035
	ntFileCreateAccess    = ntReadControl | ntFileListDirectory | ntFileReadAttributes | ntFileReadEA | ntSynchronize
	ntFileCreateOptions   = ntFileDirectoryFile | ntFileOpenReparsePoint | ntFileOpenForBackupIntent | ntFileSynchronousIONonalert
)

var (
	ntCreateFileProcedure     = ntdll.NewProc("NtCreateFile")
	rtlNtStatusToDosErrorProc = ntdll.NewProc("RtlNtStatusToDosError")
)

func createChildDirectoryPlatform(parent *os.Root, name string, afterCreate func()) (*os.Root, os.FileInfo, error) {
	parentFile, err := parent.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("open directory creation parent handle: %w", err)
	}

	nameUTF16, err := syscall.UTF16FromString(name)
	if err != nil {
		return nil, nil, errors.Join(err, parentFile.Close())
	}
	objectName := ntUnicodeString{
		Length:        uint16((len(nameUTF16) - 1) * 2),
		MaximumLength: uint16(len(nameUTF16) * 2),
		Buffer:        &nameUTF16[0],
	}
	attributes := ntObjectAttributes{
		Length:        uint32(unsafe.Sizeof(ntObjectAttributes{})),
		RootDirectory: syscall.Handle(parentFile.Fd()),
		ObjectName:    &objectName,
		Attributes:    ntObjectDontReparse,
	}
	createdHandle := syscall.InvalidHandle
	var statusBlock ntIOStatusBlock
	status, _, _ := ntCreateFileProcedure.Call(
		uintptr(unsafe.Pointer(&createdHandle)),
		uintptr(ntFileCreateAccess),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&statusBlock)),
		0,
		uintptr(ntFileAttributeNormal),
		uintptr(ntFileShareRead|ntFileShareWrite|ntFileShareDelete),
		uintptr(ntFileCreate),
		uintptr(ntFileCreateOptions),
		0,
		0,
	)
	runtime.KeepAlive(parentFile)
	runtime.KeepAlive(nameUTF16)
	parentCloseErr := parentFile.Close()
	if status != 0 {
		var createdCloseErr error
		if createdHandle != syscall.InvalidHandle && createdHandle != 0 {
			createdCloseErr = syscall.CloseHandle(createdHandle)
		}
		return nil, nil, errors.Join(windowsCreateStatusError(status), parentCloseErr, createdCloseErr)
	}
	if parentCloseErr != nil {
		_ = syscall.CloseHandle(createdHandle)
		return nil, nil, fmt.Errorf("close directory creation parent handle: %w", parentCloseErr)
	}
	createdFile := os.NewFile(uintptr(createdHandle), name)
	if createdFile == nil {
		_ = syscall.CloseHandle(createdHandle)
		return nil, nil, fmt.Errorf("adopt created directory identity handle")
	}
	createdInfo, err := createdFile.Stat()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("inspect created directory identity: %w", err), createdFile.Close())
	}
	if !createdInfo.IsDir() {
		return nil, nil, errors.Join(fmt.Errorf("created identity is not a directory"), createdFile.Close())
	}
	if afterCreate != nil {
		afterCreate()
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, errors.Join(unprovenDirectoryOwnership(name, err), createdFile.Close())
	}
	child, openedInfo, err := openExistingChildDirectory(parent, name, before)
	if err != nil {
		return nil, nil, errors.Join(unprovenDirectoryOwnership(name, err), createdFile.Close())
	}
	if !os.SameFile(createdInfo, openedInfo) {
		_ = child.Close()
		return nil, nil, errors.Join(
			unprovenDirectoryOwnership(name, fmt.Errorf("current directory is not the atomically created identity")),
			createdFile.Close(),
		)
	}
	if err := createdFile.Close(); err != nil {
		_ = child.Close()
		return nil, nil, fmt.Errorf("close created directory identity handle: %w", err)
	}
	return child, openedInfo, nil
}

func windowsCreateStatusError(status uintptr) error {
	if uint32(status) == ntStatusNameCollision {
		return syscall.EEXIST
	}
	dosCode, _, _ := rtlNtStatusToDosErrorProc.Call(status)
	if dosCode == 0 || dosCode == 317 {
		return fmt.Errorf("NtCreateFile directory creation failed with status 0x%x", uint32(status))
	}
	return syscall.Errno(dosCode)
}
