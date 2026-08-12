package security

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

var invalidWindowsName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

var nonPublicRemotePrefixes = [...]netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}
var reservedWindowsName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[1-9\x{00B9}\x{00B2}\x{00B3}]|LPT[1-9\x{00B9}\x{00B2}\x{00B3}])$`)

const maxWindowsComponentUTF16Units = 160

// ResolveWithin returns a canonical file path only when candidate is contained
// in root and none of its existing components can redirect path resolution.
func ResolveWithin(root, candidate string) (string, error) {
	if root == "" || candidate == "" {
		return "", fmt.Errorf("root and candidate are required")
	}
	if hasWindowsDeviceNamespace(root) || hasWindowsDeviceNamespace(candidate) {
		return "", fmt.Errorf("windows device namespaces are not allowed")
	}
	if hasTraversalComponent(candidate) {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	if hasReservedWindowsPathComponent(root) || hasReservedWindowsPathComponent(candidate) {
		return "", fmt.Errorf("windows reserved device names are not allowed")
	}

	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve root path: %w", err)
	}
	candidateAbs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve candidate path: %w", err)
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return "", fmt.Errorf("compare paths: %w", err)
	}
	if rel == "." || rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("candidate must be a file within root")
	}

	rootInfo, err := os.Stat(rootAbs)
	if err != nil {
		return "", fmt.Errorf("stat root: %w", err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("root is not a directory")
	}
	if _, err := walkExistingComponents(rootAbs); err != nil {
		return "", err
	}
	nearest, err := walkExistingComponents(candidateAbs)
	if err != nil {
		return "", err
	}

	canonicalRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("canonicalize root: %w", err)
	}
	canonicalNearest, err := filepath.EvalSymlinks(nearest)
	if err != nil {
		return "", fmt.Errorf("canonicalize existing parent: %w", err)
	}
	parentRel, err := filepath.Rel(canonicalRoot, canonicalNearest)
	if err != nil || filepath.IsAbs(parentRel) || parentRel == ".." || strings.HasPrefix(parentRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("candidate resolves outside root")
	}
	tail, err := filepath.Rel(nearest, candidateAbs)
	if err != nil || filepath.IsAbs(tail) {
		return "", fmt.Errorf("resolve candidate path")
	}
	if tail == "." {
		info, err := os.Lstat(candidateAbs)
		if err != nil {
			return "", fmt.Errorf("lstat candidate: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("candidate must be a regular file")
		}
	}
	return filepath.Join(canonicalNearest, tail), nil
}

func walkExistingComponents(path string) (string, error) {
	cleaned := filepath.Clean(path)
	volume := filepath.VolumeName(cleaned)
	current := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(cleaned, current)
	if remainder == cleaned {
		return "", fmt.Errorf("path is not absolute")
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return filepath.Dir(current), nil
		}
		if err != nil {
			return "", fmt.Errorf("lstat %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 {
			return "", fmt.Errorf("path component %q is a symbolic link or reparse point", current)
		}
	}
	return current, nil
}

func hasWindowsDeviceNamespace(value string) bool {
	normalized := strings.ReplaceAll(value, "/", "\\")
	upper := strings.ToUpper(normalized)
	return strings.HasPrefix(upper, `\\?\`) ||
		strings.HasPrefix(upper, `\\.\`) ||
		strings.HasPrefix(upper, `\??\`) ||
		strings.HasPrefix(upper, `\\GLOBALROOT\DEVICE\`) ||
		strings.HasPrefix(upper, `\GLOBALROOT\DEVICE\`)
}

func hasTraversalComponent(value string) bool {
	for _, component := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if component == ".." {
			return true
		}
	}
	return false
}

func hasReservedWindowsPathComponent(value string) bool {
	for _, component := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if isReservedWindowsDeviceName(component) {
			return true
		}
	}
	return false
}

func ValidateRemoteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("remote URL must use HTTP or HTTPS")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("remote URL host is required")
	}
	if address, err := netip.ParseAddr(strings.Split(host, "%")[0]); err == nil && !IsPublicRemoteIP(address) {
		return nil, fmt.Errorf("remote URL host is not public")
	}
	return parsed, nil
}

// IsPublicRemoteIP reports whether an address is globally routable rather than
// local, special-use, documentation, benchmarking, or transition space.
func IsPublicRemoteIP(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() || address.IsMulticast() {
		return false
	}
	for _, prefix := range nonPublicRemotePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func SanitizeWindowsName(value string) string {
	reserved := isReservedWindowsDeviceName(value)
	name := strings.TrimRight(strings.TrimSpace(invalidWindowsName.ReplaceAllString(value, "_")), ". ")
	if name == "" {
		return "untitled"
	}
	if reserved || isReservedWindowsDeviceName(name) {
		name = "_" + name
	}
	name = strings.TrimRight(truncateWindowsComponent(name), ". ")
	if name == "" {
		return "untitled"
	}
	if isReservedWindowsDeviceName(name) {
		name = strings.TrimRight(truncateWindowsComponent("_"+name), ". ")
		if name == "" {
			return "untitled"
		}
	}
	return name
}

func isReservedWindowsDeviceName(value string) bool {
	return reservedWindowsName.MatchString(normalizeWindowsDeviceBaseName(value))
}

func normalizeWindowsDeviceBaseName(value string) string {
	name := strings.TrimSpace(value)
	if index := strings.IndexAny(name, `\\/:`); index >= 0 {
		name = name[:index]
	}
	name, _, _ = strings.Cut(name, ".")
	return strings.TrimRight(strings.TrimSpace(name), ". ")
}

func truncateWindowsComponent(value string) string {
	units := 0
	for index, r := range value {
		runeUnits := utf16.RuneLen(r)
		if runeUnits < 0 {
			runeUnits = 1
		}
		if units+runeUnits > maxWindowsComponentUTF16Units {
			return value[:index]
		}
		units += runeUnits
	}
	return value
}
