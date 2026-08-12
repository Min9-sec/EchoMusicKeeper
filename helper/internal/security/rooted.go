package security

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Directory anchors operations to an opened directory identity. Each parent
// component is opened and verified separately before a single-component file
// operation is issued beneath it.
type Directory struct {
	root       *os.Root
	path       string
	info       os.FileInfo
	parent     *os.Root
	parentName string
}

func OpenDirectory(path string, create bool) (*Directory, error) {
	directory, err := openDirectory(path, create)
	if err != nil {
		return nil, err
	}
	if err := directory.Verify(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

// CreateDirectory creates only the final path component and returns a
// capability for that exact new directory. Existing final components are
// rejected.
func CreateDirectory(path string) (*Directory, error) {
	return createDirectory(path, nil)
}

func createDirectory(path string, afterCreate func()) (*Directory, error) {
	absolute, anchor, components, err := absoluteComponents(path)
	if err != nil {
		return nil, err
	}
	if len(components) == 0 {
		return nil, fmt.Errorf("refuse to create a volume root")
	}
	current, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, fmt.Errorf("open directory anchor: %w", err)
	}
	for _, component := range components[:len(components)-1] {
		next, _, err := openChildDirectory(current, component, false)
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open directory component %q: %w", component, err)
		}
		_ = current.Close()
		current = next
	}
	name := components[len(components)-1]
	root, info, err := createChildDirectory(current, name, afterCreate)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create directory %q: %w", absolute, err), current.Close())
	}
	directory := &Directory{root: root, path: absolute, info: info, parent: current, parentName: name}
	if err := directory.Verify(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func openDirectory(path string, create bool) (*Directory, error) {
	absolute, anchor, components, err := absoluteComponents(path)
	if err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, fmt.Errorf("open directory anchor: %w", err)
	}
	if len(components) == 0 {
		info, err := rootInfo(current)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		return &Directory{root: current, path: absolute, info: info}, nil
	}
	for index, component := range components {
		next, info, err := openChildDirectory(current, component, create)
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open directory component %q: %w", component, err)
		}
		if index == len(components)-1 {
			return &Directory{
				root: next, path: absolute, info: info,
				parent: current, parentName: component,
			}, nil
		}
		_ = current.Close()
		current = next
	}
	panic("unreachable")
}

func absoluteComponents(path string) (string, string, []string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", nil, fmt.Errorf("directory path is required")
	}
	if hasWindowsDeviceNamespace(path) {
		return "", "", nil, fmt.Errorf("windows device namespaces are not allowed")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve directory path: %w", err)
	}
	volume := filepath.VolumeName(absolute)
	anchor := volume + string(filepath.Separator)
	if volume == "" {
		anchor = string(filepath.Separator)
	}
	remainder := strings.TrimPrefix(absolute, anchor)
	if remainder == absolute {
		return "", "", nil, fmt.Errorf("directory path is not absolute")
	}
	components := make([]string, 0)
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component != "" && component != "." {
			components = append(components, component)
		}
	}
	return absolute, anchor, components, nil
}

// OpenChildDirectory opens one real directory directly beneath parent. When
// create is true, a missing child is created under the platform ownership
// guarantee before its capability is returned.
func OpenChildDirectory(parent *os.Root, name string, create bool) (*os.Root, os.FileInfo, error) {
	return openChildDirectory(parent, name, create)
}

func openChildDirectory(parent *os.Root, name string, create bool) (*os.Root, os.FileInfo, error) {
	before, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		return createChildDirectory(parent, name, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	return openExistingChildDirectory(parent, name, before)
}

func openExistingChildDirectory(parent *os.Root, name string, before os.FileInfo) (*os.Root, os.FileInfo, error) {
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("component is not a real directory")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := rootInfo(child)
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	after, err := parent.Lstat(name)
	if err != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(after, opened) {
		_ = child.Close()
		return nil, nil, fmt.Errorf("directory component changed while it was opened")
	}
	return child, opened, nil
}

func createChildDirectory(parent *os.Root, name string, afterCreate func()) (*os.Root, os.FileInfo, error) {
	parentBefore, err := rootInfo(parent)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect directory creation parent: %w", err)
	}
	if _, err := parent.Lstat(name); err == nil {
		return nil, nil, fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	child, info, err := createChildDirectoryPlatform(parent, name, afterCreate)
	if err != nil {
		return nil, nil, err
	}
	parentAfter, parentErr := rootInfo(parent)
	if parentErr != nil || !os.SameFile(parentBefore, parentAfter) {
		_ = child.Close()
		return nil, nil, errors.Join(fmt.Errorf("directory creation parent identity changed"), parentErr)
	}
	return child, info, nil
}

func unprovenDirectoryOwnership(name string, cause error) error {
	return errors.Join(
		fmt.Errorf("created directory %q ownership could not be proven: %w", name, cause),
		fmt.Errorf("current basename was left untouched; created directory identity location is unknown"),
	)
}

func rootInfo(root *os.Root) (os.FileInfo, error) {
	opened, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("inspect opened directory: %w", err)
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("opened path is not a directory")
	}
	return info, nil
}

// OpenVolume opens the volume anchor containing path and returns path relative
// to that anchor. Operations on the returned Directory still verify every
// relative parent component separately.
func OpenVolume(path string) (*Directory, string, error) {
	absolute, anchor, _, err := absoluteComponents(path)
	if err != nil {
		return nil, "", err
	}
	directory, err := OpenDirectory(anchor, false)
	if err != nil {
		return nil, "", err
	}
	relative, err := directory.Relative(absolute)
	if err != nil {
		_ = directory.Close()
		return nil, "", err
	}
	return directory, relative, nil
}

func (directory *Directory) Path() string {
	return directory.path
}

func (directory *Directory) Identity() os.FileInfo {
	return directory.info
}

// CloneRoot returns a new capability for the same opened directory identity.
// The caller owns the returned root.
func (directory *Directory) CloneRoot() (*os.Root, error) {
	if directory == nil || directory.root == nil || directory.info == nil {
		return nil, fmt.Errorf("anchored directory is closed")
	}
	clone, err := directory.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	info, err := rootInfo(clone)
	if err != nil || !os.SameFile(info, directory.info) {
		_ = clone.Close()
		return nil, fmt.Errorf("cloned directory identity changed")
	}
	return clone, nil
}

func (directory *Directory) Close() error {
	if directory == nil {
		return nil
	}
	var result error
	if directory.root != nil {
		result = errors.Join(result, directory.root.Close())
		directory.root = nil
	}
	if directory.parent != nil {
		result = errors.Join(result, directory.parent.Close())
		directory.parent = nil
	}
	return result
}

func (directory *Directory) Verify() error {
	if directory == nil || directory.root == nil || directory.info == nil {
		return fmt.Errorf("anchored directory is closed")
	}
	openedInfo, err := rootInfo(directory.root)
	if err != nil || !os.SameFile(openedInfo, directory.info) {
		return fmt.Errorf("anchored directory identity changed")
	}
	reopened, err := openDirectory(directory.path, false)
	if err != nil {
		return fmt.Errorf("anchored directory path changed: %w", err)
	}
	reopenedInfo := reopened.info
	closeErr := reopened.Close()
	if closeErr != nil {
		return closeErr
	}
	if !os.SameFile(reopenedInfo, directory.info) {
		return fmt.Errorf("anchored directory path changed")
	}
	return nil
}

func (directory *Directory) Relative(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(directory.path, absolute)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside anchored directory")
	}
	if relative == "." {
		return ".", nil
	}
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("path is not local to anchored directory")
	}
	return relative, nil
}

func localComponents(name string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("relative path is required")
	}
	cleaned := filepath.Clean(name)
	if cleaned == "." {
		return nil, nil
	}
	if !filepath.IsLocal(cleaned) || filepath.IsAbs(cleaned) || hasWindowsDeviceNamespace(cleaned) {
		return nil, fmt.Errorf("path is not local to anchored directory")
	}
	components := strings.Split(cleaned, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("invalid relative path component")
		}
	}
	return components, nil
}

func (directory *Directory) openParent(name string, verifyPath bool) (*os.Root, string, bool, error) {
	if directory == nil || directory.root == nil {
		return nil, "", false, fmt.Errorf("anchored directory is closed")
	}
	if verifyPath {
		if err := directory.Verify(); err != nil {
			return nil, "", false, err
		}
	}
	components, err := localComponents(name)
	if err != nil {
		return nil, "", false, err
	}
	if len(components) == 0 {
		return directory.root, ".", false, nil
	}
	current := directory.root
	owned := false
	for _, component := range components[:len(components)-1] {
		next, _, err := openChildDirectory(current, component, false)
		if err != nil {
			if owned {
				_ = current.Close()
			}
			return nil, "", false, err
		}
		if owned {
			_ = current.Close()
		}
		current = next
		owned = true
	}
	return current, components[len(components)-1], owned, nil
}

func closeOwned(root *os.Root, owned bool) error {
	if !owned {
		return nil
	}
	return root.Close()
}

func (directory *Directory) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return nil, err
	}
	defer closeOwned(parent, owned)
	before, beforeErr := parent.Lstat(basename)
	if beforeErr == nil && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("path is not a regular file")
	}
	if beforeErr != nil && !errors.Is(beforeErr, fs.ErrNotExist) {
		return nil, beforeErr
	}
	truncate := flag&os.O_TRUNC != 0
	file, err := parent.OpenFile(basename, flag&^os.O_TRUNC, perm)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	after, afterErr := parent.Lstat(basename)
	if statErr != nil || afterErr != nil || !opened.Mode().IsRegular() ||
		after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) ||
		(beforeErr == nil && !os.SameFile(before, opened)) {
		_ = file.Close()
		return nil, fmt.Errorf("file changed while it was opened")
	}
	if truncate {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if err := directory.Verify(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (directory *Directory) Lstat(name string) (os.FileInfo, error) {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return nil, err
	}
	defer closeOwned(parent, owned)
	return parent.Lstat(basename)
}

func (directory *Directory) Rename(oldName, newName string) error {
	oldParent, oldBase, oldOwned, err := directory.openParent(oldName, true)
	if err != nil {
		return err
	}
	defer closeOwned(oldParent, oldOwned)
	newParent, newBase, newOwned, err := directory.openParent(newName, true)
	if err != nil {
		return err
	}
	defer closeOwned(newParent, newOwned)
	oldInfo, err := oldParent.Lstat(oldBase)
	if err != nil || oldInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rename source is not a real filesystem entry")
	}
	if _, err := newParent.Lstat(newBase); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("rename destination already exists")
		}
		return err
	}
	if err := renameBetweenRoots(oldParent, oldBase, newParent, newBase); err != nil {
		return err
	}
	renamed, err := newParent.Lstat(newBase)
	if err != nil || !os.SameFile(oldInfo, renamed) {
		return fmt.Errorf("renamed entry identity changed")
	}
	return directory.Verify()
}

func (directory *Directory) Remove(name string) error {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return err
	}
	defer closeOwned(parent, owned)
	info, err := parent.Lstat(basename)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove a symbolic link")
	}
	if err := parent.Remove(basename); err != nil {
		return err
	}
	return directory.Verify()
}

// Cleanup removes a caller-created entry through the original directory
// capability even when the directory's presentation path has been replaced.
func (directory *Directory) Cleanup(name string) error {
	parent, basename, owned, err := directory.openParent(name, false)
	if err != nil {
		return err
	}
	defer closeOwned(parent, owned)
	info, err := parent.Lstat(basename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to clean a symbolic link")
	}
	return parent.Remove(basename)
}

// CleanupContents recursively removes caller-owned entries through the
// original directory capability without consulting its presentation path.
func (directory *Directory) CleanupContents() error {
	if directory == nil || directory.root == nil {
		return fmt.Errorf("anchored directory is closed")
	}
	entries, err := readRoot(directory.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeAllEntry(directory.root, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (directory *Directory) RemoveAll(name string) error {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return err
	}
	defer closeOwned(parent, owned)
	if err := removeAllEntry(parent, basename); err != nil {
		return err
	}
	return directory.Verify()
}

func removeAllEntry(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return parent.Remove(name)
	}
	child, openedInfo, err := openChildDirectory(parent, name, false)
	if err != nil {
		return err
	}
	entries, readErr := readRoot(child)
	if readErr == nil {
		for _, entry := range entries {
			if err := removeAllEntry(child, entry.Name()); err != nil {
				readErr = err
				break
			}
		}
	}
	closeErr := child.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	current, err := parent.Lstat(name)
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, current) {
		return fmt.Errorf("directory changed during recursive removal")
	}
	return parent.Remove(name)
}

func (directory *Directory) RemoveRegular(name string) error {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return err
	}
	defer closeOwned(parent, owned)
	info, err := parent.Lstat(basename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path is not a regular file")
	}
	file, err := parent.OpenFile(basename, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	current, currentErr := parent.Lstat(basename)
	if err := errors.Join(statErr, closeErr, currentErr); err != nil {
		return err
	}
	if !os.SameFile(info, opened) || !os.SameFile(opened, current) || current.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("regular file changed before removal")
	}
	if err := parent.Remove(basename); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return directory.Verify()
}

func (directory *Directory) ReadDir(name string) ([]os.DirEntry, error) {
	parent, basename, owned, err := directory.openParent(name, true)
	if err != nil {
		return nil, err
	}
	defer closeOwned(parent, owned)
	if basename == "." {
		return readRoot(parent)
	}
	child, _, err := openChildDirectory(parent, basename, false)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	return readRoot(child)
}

func readRoot(root *os.Root) ([]os.DirEntry, error) {
	opened, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := opened.ReadDir(-1)
	return entries, errors.Join(readErr, opened.Close())
}

// RemoveOpenedEmpty removes this opened directory by identity when the target
// platform supports handle-bound directory disposition. Other platforms leave
// the verified empty directory in place and return removed=false.
func (directory *Directory) RemoveOpenedEmpty(afterCheck func()) (bool, error) {
	if directory == nil || directory.root == nil || directory.info == nil {
		return false, fmt.Errorf("anchored directory is closed")
	}
	entries, err := readRoot(directory.root)
	if err != nil {
		return false, err
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("opened directory is not empty")
	}
	return removeOpenedDirectory(directory.root, afterCheck)
}
