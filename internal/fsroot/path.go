// Package fsroot confines every filesystem operation requested by a guest to
// the shared directory.
//
// Two layers of defence are used:
//
//  1. Guest-supplied paths are validated lexically by Clean. Anything that is
//     not a plain, portable, relative path (traversal, absolute paths,
//     backslashes, NUL, Windows device names, alternate data streams, ...) is
//     rejected before touching the filesystem.
//  2. All access goes through os.Root, which resolves paths relative to an open
//     directory handle and refuses to follow ".." or symlinks out of the
//     root. Because resolution is done by the kernel relative to a handle
//     (openat / NtCreateFile with a root handle), this is not vulnerable to
//     time-of-check/time-of-use races where a directory is swapped for a
//     symlink between validation and use.
package fsroot

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidPath is returned for any guest path that fails validation.
var ErrInvalidPath = errors.New("invalid path")

// InternalDirName is the hidden directory inside the share used for
// incomplete uploads. It lives inside the share so finalisation is an atomic
// same-filesystem rename. Guests can never address it.
const InternalDirName = ".sharebox-uploads"

const (
	maxComponentBytes = 255
	maxPathBytes      = 4096
	maxDepth          = 128
)

// Clean validates a slash-separated guest path and returns it in canonical
// form ("." for the root, otherwise "a/b/c" with no leading or trailing
// slash). The same rules apply on every host OS so that a share behaves
// identically on Windows and macOS.
func Clean(p string) (string, error) {
	if len(p) > maxPathBytes || !utf8.ValidString(p) {
		return "", ErrInvalidPath
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", ErrInvalidPath
	}
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if err := ValidName(c); err != nil {
			return "", err
		}
		out = append(out, c)
	}
	if len(out) > maxDepth {
		return "", ErrInvalidPath
	}
	if len(out) == 0 {
		return ".", nil
	}
	return strings.Join(out, "/"), nil
}

// ValidName validates a single path component (a file or folder name).
func ValidName(c string) error {
	if c == "" || c == "." || c == ".." || len(c) > maxComponentBytes || !utf8.ValidString(c) {
		return ErrInvalidPath
	}
	for _, r := range c {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cc, r) {
			return ErrInvalidPath
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			// ':' also blocks NTFS alternate data streams (file.txt:secret).
			return ErrInvalidPath
		}
	}
	// Windows silently strips trailing dots and spaces, which would let
	// "secret.txt." alias "secret.txt". Reject them everywhere.
	if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return ErrInvalidPath
	}
	if isReservedWindowsName(c) {
		return ErrInvalidPath
	}
	if strings.EqualFold(c, InternalDirName) {
		return ErrInvalidPath
	}
	return nil
}

func isReservedWindowsName(c string) bool {
	base := c
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(strings.ToUpper(base), " ")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		d := base[3]
		// COM0/LPT0 and the superscript digits are reserved too.
		return d >= '0' && d <= '9'
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		rest := []rune(base[3:])
		return len(rest) == 1 && (rest[0] == '¹' || rest[0] == '²' || rest[0] == '³')
	}
	return false
}

// SanitizeName turns an arbitrary client-supplied file name into a valid one,
// replacing forbidden characters. It is used for uploads so that a file named
// "a:b.txt" from a Linux guest still lands on a Windows host. It returns an
// error only if nothing usable remains.
func SanitizeName(name string) (string, error) {
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "_")
	}
	// Browsers may send a full path for directory uploads; take the base.
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			continue
		case strings.ContainsRune(`:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	for len(s) > maxComponentBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	if s == "" || s == "." || s == ".." {
		return "", ErrInvalidPath
	}
	if isReservedWindowsName(s) {
		s = "_" + s
	}
	if err := ValidName(s); err != nil {
		return "", err
	}
	return s, nil
}

// Join joins a cleaned directory and a validated name.
func Join(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// Split returns the parent directory and base name of a cleaned path.
func Split(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ".", p
	}
	return p[:i], p[i+1:]
}
