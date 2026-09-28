package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestResolveWithin(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "audio", "song.mp3")
	if err := os.Mkdir(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveWithin(root, existing)
	if err != nil || got != existing {
		t.Fatalf("expected existing file %q, got %q, %v", existing, got, err)
	}

	missing := filepath.Join(root, "audio", "new-song.mp3")
	got, err = ResolveWithin(root, missing)
	if err != nil || got != missing {
		t.Fatalf("expected nearest-existing-parent resolution for %q, got %q, %v", missing, got, err)
	}

	for _, candidate := range []string{
		root,
		filepath.Dir(existing),
		filepath.Join(root, "..", "escape.mp3"),
		`\\?\C:\cache\song.mp3`,
		`\\.\C:\cache\song.mp3`,
		`\??\C:\cache\song.mp3`,
		`\\GLOBALROOT\Device\HarddiskVolume1\song.mp3`,
		filepath.Join(root, "CONIN$", "song.mp3"),
		filepath.Join(root, "CONOUT$.txt"),
		filepath.Join(root, "CON .txt"),
		filepath.Join(root, "COM1 .log"),
		filepath.Join(root, "COM\u00b9.txt"),
		filepath.Join(root, "LPT\u00b3.txt"),
	} {
		if _, err := ResolveWithin(root, candidate); err == nil {
			t.Fatalf("expected rejection for %q", candidate)
		}
	}
}

func TestWindowsPathSyntaxClassifier(t *testing.T) {
	for _, value := range []string{
		`C:\cache\song.mp3`,
		`C:/cache/song.mp3`,
		`\\server\share\song.mp3`,
		`//server/share/song.mp3`,
		`\\GLOBALROOT\share\song.mp3`,
	} {
		if hasWindowsDeviceNamespace(value) {
			t.Fatalf("expected ordinary Windows path syntax to be safe: %q", value)
		}
	}

	for _, value := range []string{
		`\\?\C:\cache\song.mp3`,
		`\\.\C:\cache\song.mp3`,
		`\??\C:\cache\song.mp3`,
		`\\GLOBALROOT\Device\HarddiskVolume1\song.mp3`,
	} {
		if !hasWindowsDeviceNamespace(value) {
			t.Fatalf("expected Windows device namespace rejection: %q", value)
		}
	}
}

func TestResolveWithinRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(target, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "song.mp3")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := ResolveWithin(root, link); err == nil {
		t.Fatal("expected symbolic-link rejection")
	}

	directoryLink := filepath.Join(root, "audio")
	if err := os.Symlink(filepath.Dir(target), directoryLink); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}
	if _, err := ResolveWithin(root, filepath.Join(directoryLink, "song.mp3")); err == nil {
		t.Fatal("expected symbolic-link parent rejection")
	}
}

func TestValidateRemoteURL(t *testing.T) {
	if _, err := ValidateRemoteURL("https://example.test/song.mp3?token=secret"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"file:///C:/secret",
		"ftp://example.test/song",
		"http://127.0.0.1/a",
		"https://[::1]/audio",
		"https://[fc00::1]/audio",
		"https://[fe80::1]/audio",
		"https://[fe80::1%25Ethernet]/audio",
		"https://[::1%25Ethernet]/audio",
		"https://[fc00::1%25Ethernet]/audio",
		"http://100.64.0.1/audio",
		"http://198.18.1.175/audio",
		"http://[64:ff9b::a00:1]/audio",
		"http://192.0.2.1/audio",
		"http://224.0.0.1/audio",
		"https://[2001::1]/audio",
		"https://[2001:db8::1]/audio",
		"https://[2002:7f00:1::]/audio",
	} {
		if _, err := ValidateRemoteURL(raw); err == nil {
			t.Fatalf("expected rejection for %s", raw)
		}
	}
}

func TestSanitizeWindowsName(t *testing.T) {
	for input, want := range map[string]string{
		`CON: bad/song. `: "_CON_ bad_song",
		"CONIN$":          "_CONIN$",
		"CONOUT$.txt":     "_CONOUT$.txt",
		"CON .txt":        "_CON .txt",
		"COM1 .log":       "_COM1 .log",
		"COM\u00b9.txt":   "_COM\u00b9.txt",
		"LPT\u00b3":       "_LPT\u00b3",
	} {
		if got := SanitizeWindowsName(input); got != want {
			t.Fatalf("SanitizeWindowsName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSanitizeWindowsNameUsesUTF16Limit(t *testing.T) {
	name := SanitizeWindowsName(strings.Repeat("a", 159) + ".b")
	if name != strings.Repeat("a", 159) {
		t.Fatalf("unexpected trailing-dot truncation result %q", name)
	}

	name = SanitizeWindowsName(strings.Repeat("a", 159) + "\U0001f600")
	if name != strings.Repeat("a", 159) {
		t.Fatalf("unexpected UTF-16 truncation result %q", name)
	}
	if units := len(utf16.Encode([]rune(name))); units > 160 {
		t.Fatalf("name has %d UTF-16 units, want at most 160", units)
	}

	name = SanitizeWindowsName("CON" + strings.Repeat(" ", 157) + "x")
	if name != "_CON" {
		t.Fatalf("unexpected reserved-name truncation result %q", name)
	}

	name = SanitizeWindowsName(strings.Repeat(".", 161) + "x")
	if name != "untitled" {
		t.Fatalf("unexpected empty truncation result %q", name)
	}
}
