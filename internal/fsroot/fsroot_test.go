package fsroot

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCleanRejectsTraversalAndOddNames(t *testing.T) {
	bad := []string{
		"..", "../x", "a/../../b", "a/..", "/../etc/passwd", "a\\..\\b", "C:\\Windows", "c:/windows",
		"a\x00b", "file.txt:stream", "CON", "con.txt", "a/NUL", "LPT1.log", "COM9", "trail.", "trail ",
		".sharebox-uploads", "x/.SHAREBOX-UPLOADS", "a/\x01", "\\\\server\\share", "a/b\\c",
	}
	for _, p := range bad {
		if _, err := Clean(p); err == nil {
			t.Errorf("Clean(%q) accepted", p)
		}
	}
	good := map[string]string{
		"": ".", "/": ".", ".": ".", "a": "a", "/a/b/": "a/b", "a//b/./c": "a/b/c",
		"Ünïcode 文件.txt": "Ünïcode 文件.txt", ".hidden": ".hidden", "a..b": "a..b", "CONSOLE": "CONSOLE",
	}
	for in, want := range good {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"a:b?.txt":         "a_b_.txt",
		"../../evil.sh":    "evil.sh",
		"C:\\x\\photo.jpg": "photo.jpg",
		"con":              "_con",
		"name. ":           "name",
	}
	for in, want := range cases {
		got, err := SanitizeName(in)
		if err != nil || got != want {
			t.Errorf("SanitizeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "..", "/", " . "} {
		if _, err := SanitizeName(in); err == nil {
			t.Errorf("SanitizeName(%q) accepted", in)
		}
	}
}

func setup(t *testing.T) (*Root, string, string) {
	t.Helper()
	base := t.TempDir()
	share := filepath.Join(base, "share")
	outside := filepath.Join(base, "outside")
	must(t, os.MkdirAll(filepath.Join(share, "sub"), 0o755))
	must(t, os.MkdirAll(outside, 0o755))
	must(t, os.WriteFile(filepath.Join(share, "sub", "in.txt"), []byte("inside"), 0o644))
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644))
	r, err := Open(share)
	must(t, err)
	t.Cleanup(func() { r.Close() })
	return r, share, outside
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkEscapesAreBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	r, share, outside := setup(t)
	must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(share, "link-file")))
	must(t, os.Symlink(outside, filepath.Join(share, "link-dir")))
	must(t, os.Symlink("../outside/secret.txt", filepath.Join(share, "rel-link")))
	must(t, os.Symlink("sub/in.txt", filepath.Join(share, "ok-link")))

	for _, p := range []string{"link-file", "link-dir/secret.txt", "rel-link"} {
		if f, _, err := r.OpenFile(p); err == nil {
			f.Close()
			t.Errorf("OpenFile(%q) escaped the share", p)
		}
	}
	if _, err := r.List("link-dir"); err == nil {
		t.Error("List through escaping symlink succeeded")
	}
	// A link inside the share keeps working.
	f, _, err := r.OpenFile("ok-link")
	must(t, err)
	f.Close()

	// Escaping links are hidden from listings.
	es, err := r.List("")
	must(t, err)
	names := map[string]bool{}
	for _, e := range es {
		names[e.Name] = true
	}
	if names["link-file"] || names["link-dir"] || names["rel-link"] || !names["ok-link"] || !names["sub"] {
		t.Errorf("unexpected listing: %v", names)
	}

	// Deleting a symlinked directory must remove only the link.
	must(t, r.Remove("link-dir"))
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("deleting a link removed the target")
	}
}

func TestSwappedDirectoryRace(t *testing.T) {
	// Simulates a TOCTOU attack: a folder that existed when listed is
	// replaced by a symlink pointing outside before it is opened.
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	r, share, outside := setup(t)
	if _, err := r.List("sub"); err != nil {
		t.Fatal(err)
	}
	must(t, os.RemoveAll(filepath.Join(share, "sub")))
	must(t, os.Symlink(outside, filepath.Join(share, "sub")))
	if f, _, err := r.OpenFile("sub/secret.txt"); err == nil {
		f.Close()
		t.Fatal("followed swapped symlink outside the share")
	}
}

func TestInternalDirHidden(t *testing.T) {
	r, _, _ := setup(t)
	ir, err := r.Internal()
	must(t, err)
	ir.Close()
	es, _ := r.List("")
	for _, e := range es {
		if e.Name == InternalDirName {
			t.Fatal("internal dir listed")
		}
	}
	if _, err := r.Stat(InternalDirName); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("internal dir addressable: %v", err)
	}
}

func TestRootCannotBeDeleted(t *testing.T) {
	r, _, _ := setup(t)
	for _, p := range []string{"", ".", "/"} {
		if err := r.Remove(p); err == nil {
			t.Fatalf("Remove(%q) succeeded", p)
		}
	}
}
