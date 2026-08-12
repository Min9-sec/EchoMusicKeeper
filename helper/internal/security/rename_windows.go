//go:build windows

package security

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	ntSynchronize                = 0x00100000
	ntDelete                     = 0x00010000
	ntFileShareRead              = 0x00000001
	ntFileShareWrite             = 0x00000002
	ntFileShareDelete            = 0x00000004
	ntFileOpenReparsePoint       = 0x00200000
	ntFileOpenForBackupIntent    = 0x00004000
	ntFileSynchronousIONonalert  = 0x00000020
	ntFileRenameInformationClass = 10
)

var (
	ntdll                    = syscall.NewLazyDLL("ntdll.dll")
	ntOpenFileProcedure      = ntdll.NewProc("NtOpenFile")
	ntSetInformationFileProc = ntdll.NewProc("NtSetInformationFile")
)

type ntUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type ntObjectAttributes struct {
	Length             uint32
	RootDirectory      syscall.Handle
	ObjectName         *ntUnicodeString
	Attributes         uint32
	SecurityDescriptor unsafe.Pointer
	SecurityQoS        unsafe.Pointer
}

type ntIOStatusBlock struct {
	Status      uintptr
	Information uintptr
}

type ntFileRenameInformation struct {
	ReplaceIfExists bool
	RootDirectory   syscall.Handle
	FileNameLength  uint32
	FileName        [syscall.MAX_PATH]uint16
}

func renameBetweenRoots(oldRoot *os.Root, oldName string, newRoot *os.Root, newName string) error {
	oldParent, err := oldRoot.Open(".")
	if err != nil {
		return err
	}
	defer oldParent.Close()
	newParent, err := newRoot.Open(".")
	if err != nil {
		return err
	}
	defer newParent.Close()

	oldUTF16, err := syscall.UTF16FromString(oldName)
	if err != nil {
		return err
	}
	oldString := ntUnicodeString{
		Length: uint16((len(oldUTF16) - 1) * 2), MaximumLength: uint16(len(oldUTF16) * 2), Buffer: &oldUTF16[0],
	}
	attributes := ntObjectAttributes{
		Length:        uint32(unsafe.Sizeof(ntObjectAttributes{})),
		RootDirectory: syscall.Handle(oldParent.Fd()), ObjectName: &oldString,
	}
	var source syscall.Handle
	var openStatus ntIOStatusBlock
	status, _, _ := ntOpenFileProcedure.Call(
		uintptr(unsafe.Pointer(&source)),
		uintptr(ntSynchronize|ntDelete),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&openStatus)),
		uintptr(ntFileShareDelete|ntFileShareRead|ntFileShareWrite),
		uintptr(ntFileOpenReparsePoint|ntFileOpenForBackupIntent|ntFileSynchronousIONonalert),
	)
	runtime.KeepAlive(oldUTF16)
	if status != 0 {
		return fmt.Errorf("NtOpenFile rename source failed with status 0x%x", uint32(status))
	}
	defer syscall.CloseHandle(source)

	newUTF16, err := syscall.UTF16FromString(newName)
	if err != nil {
		return err
	}
	if len(newUTF16) > syscall.MAX_PATH {
		return syscall.ENAMETOOLONG
	}
	information := ntFileRenameInformation{RootDirectory: syscall.Handle(newParent.Fd())}
	copy(information.FileName[:], newUTF16)
	information.FileNameLength = uint32((len(newUTF16) - 1) * 2)
	var renameStatus ntIOStatusBlock
	status, _, _ = ntSetInformationFileProc.Call(
		uintptr(source),
		uintptr(unsafe.Pointer(&renameStatus)),
		uintptr(unsafe.Pointer(&information)),
		uintptr(unsafe.Sizeof(information)),
		uintptr(ntFileRenameInformationClass),
	)
	runtime.KeepAlive(newUTF16)
	if status != 0 {
		return fmt.Errorf("NtSetInformationFile rename failed with status 0x%x", uint32(status))
	}
	return nil
}
