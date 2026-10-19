package fsroot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Errors surfaced to guests. They deliberately never contain host paths.
var (
	ErrNotFound    = errors.New("not found")
	ErrNotDir      = errors.New("not a folder")
	ErrIsDir       = errors.New("is a folder")
	ErrExists      = errors.New("already exists")
	ErrNotRegular  = errors.New("not a regular file")
	ErrTooManyHits = errors.New("too many entries")
)

// Entry describes a file or folder in the share. Paths are guest-relative.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Root is a handle on the shared directory.
type Root struct {
	r       *os.Root
	absPath string // host path; never sent to guests
	name    string // display name (base name of the folder)
}

// Open opens dir as a share root.
func Open(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrNotDir
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(abs)
	if name == string(filepath.Separator) || name == "." || strings.HasSuffix(name, ":\\") {
		name = "Shared folder"
	}
	return &Root{r: r, absPath: abs, name: name}, nil
}

// Close releases the root handle.
func (r *Root) Close() error { return r.r.Close() }

// Name is the display name of the share.
func (r *Root) Name() string { return r.name }

// HostPath returns the absolute host path. Only the host UI may use it.
func (r *Root) HostPath() string { return r.absPath }

func osPath(p string) string { return filepath.FromSlash(p) }

// checkInternal rejects any path whose first component resolves to the
// internal uploads directory, even through an alias such as a Windows 8.3
// short name or a case variation that the lexical check could miss.
func (r *Root) checkInternal(p string) error {
	if p == "." {
		return nil
	}
	first, _, _ := strings.Cut(p, "/")
	fi, err := r.r.Lstat(osPath(first))
	if err != nil {
		return nil // not existing is handled by the caller's own operation
	}
	ifi, err := r.r.Lstat(InternalDirName)
	if err != nil {
		return nil
	}
	if os.SameFile(fi, ifi) {
		return ErrInvalidPath
	}
	return nil
}

// Resolve cleans a guest path and checks it does not address internal state.
func (r *Root) Resolve(p string) (string, error) {
	c, err := Clean(p)
	if err != nil {
		return "", err
	}
	if err := r.checkInternal(c); err != nil {
		return "", err
	}
	return c, nil
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	case errors.Is(err, ErrInvalidPath), errors.Is(err, ErrNotDir), errors.Is(err, ErrIsDir),
		errors.Is(err, ErrNotRegular), errors.Is(err, ErrExists), errors.Is(err, ErrNotFound):
		return err
	}
	// os.Root reports escapes ("path escapes from parent") as a *PathError.
	var pe *fs.PathError
	if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
		return ErrInvalidPath
	}
	if errors.Is(err, fs.ErrPermission) {
		return fs.ErrPermission
	}
	// Strip the host path from any other error.
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// Stat returns information about a guest path. Symlinks are followed only
// while they stay inside the share.
func (r *Root) Stat(p string) (Entry, error) {
	c, err := r.Resolve(p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := r.r.Stat(osPath(c))
	if err != nil {
		return Entry{}, mapErr(err)
	}
	return toEntry(c, fi), nil
}

func toEntry(p string, fi fs.FileInfo) Entry {
	e := Entry{Name: fi.Name(), Path: p, IsDir: fi.IsDir(), ModTime: fi.ModTime().UTC()}
	if p == "." {
		e.Path = ""
		e.Name = ""
	} else {
		_, e.Name = Split(p)
	}
	if !e.IsDir {
		e.Size = fi.Size()
	}
	return e
}

// MaxListEntries bounds a single directory listing.
const MaxListEntries = 50000

// List returns the visible children of a folder.
func (r *Root) List(p string) ([]Entry, error) {
	c, err := r.Resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := r.r.Open(osPath(c))
	if err != nil {
		return nil, mapErr(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, mapErr(err)
	}
	if !fi.IsDir() {
		return nil, ErrNotDir
	}
	var out []Entry
	for {
		des, err := f.ReadDir(1024)
		for _, de := range des {
			if e, ok := r.childEntry(c, de); ok {
				out = append(out, e)
			}
			if len(out) >= MaxListEntries {
				return out, ErrTooManyHits
			}
		}
		if err == io.EOF || len(des) == 0 {
			break
		}
		if err != nil {
			return nil, mapErr(err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// childEntry converts a directory entry, hiding entries guests must not see:
// the internal directory, names that are not valid portable names, and
// symlinks that point outside the share (or are broken).
func (r *Root) childEntry(dir string, de fs.DirEntry) (Entry, bool) {
	name := de.Name()
	if dir == "." && strings.EqualFold(name, InternalDirName) {
		return Entry{}, false
	}
	if ValidName(name) != nil {
		return Entry{}, false
	}
	p := Join(dir, name)
	if de.Type()&fs.ModeSymlink != 0 {
		fi, err := r.r.Stat(osPath(p)) // fails if the link escapes the root
		if err != nil {
			return Entry{}, false
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return Entry{}, false
		}
		return toEntry(p, fi), true
	}
	if !de.Type().IsDir() && !de.Type().IsRegular() {
		return Entry{}, false // sockets, devices, FIFOs
	}
	fi, err := de.Info()
	if err != nil {
		return Entry{}, false
	}
	return toEntry(p, fi), true
}

// OpenFile opens a regular file for reading. The type check is performed on
// the opened handle so the file cannot be swapped after validation.
func (r *Root) OpenFile(p string) (*os.File, fs.FileInfo, error) {
	c, err := r.Resolve(p)
	if err != nil {
		return nil, nil, err
	}
	if c == "." {
		return nil, nil, ErrIsDir
	}
	f, err := r.r.Open(osPath(c))
	if err != nil {
		return nil, nil, mapErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, mapErr(err)
	}
	if fi.IsDir() {
		f.Close()
		return nil, nil, ErrIsDir
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotRegular
	}
	return f, fi, nil
}

// Mkdir creates a single folder. The parent must exist.
func (r *Root) Mkdir(p string) (Entry, error) {
	c, err := r.Resolve(p)
	if err != nil {
		return Entry{}, err
	}
	if c == "." {
		return Entry{}, ErrExists
	}
	if err := r.r.Mkdir(osPath(c), 0o755); err != nil {
		return Entry{}, mapErr(err)
	}
	return r.Stat(c)
}

// Remove deletes a file or an entire folder tree. Symlinks are removed, never
// followed (os.Root.RemoveAll does not traverse links).
func (r *Root) Remove(p string) error {
	c, err := r.Resolve(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrInvalidPath
	}
	if _, err := r.r.Lstat(osPath(c)); err != nil {
		return mapErr(err)
	}
	return mapErr(r.r.RemoveAll(osPath(c)))
}

// Walk visits every visible entry below p (depth first, not including p).
// fn may return fs.SkipDir for a folder. Walk stops on ctx cancellation.
func (r *Root) Walk(ctx context.Context, p string, fn func(e Entry) error) error {
	c, err := r.Resolve(p)
	if err != nil {
		return err
	}
	fi, err := r.r.Stat(osPath(c))
	if err != nil {
		return mapErr(err)
	}
	if !fi.IsDir() {
		return ErrNotDir
	}
	return r.walk(ctx, c, fn, 0)
}

func (r *Root) walk(ctx context.Context, dir string, fn func(Entry) error, depth int) error {
	if depth > maxDepth {
		return nil
	}
	entries, err := r.List(dir)
	if err != nil && !errors.Is(err, ErrTooManyHits) {
		// Unreadable subfolders are skipped rather than failing the walk.
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		ferr := fn(e)
		if ferr == fs.SkipDir {
			continue
		}
		if ferr != nil {
			return ferr
		}
		if e.IsDir {
			// Do not descend through symlinked folders to avoid cycles.
			if lfi, err := r.r.Lstat(osPath(e.Path)); err != nil || lfi.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			if err := r.walk(ctx, e.Path, fn, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Internal gives the upload manager access to the hidden temp directory.
func (r *Root) Internal() (*os.Root, error) {
	if err := r.r.Mkdir(InternalDirName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	fi, err := r.r.Lstat(InternalDirName)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("internal upload directory is not a directory")
	}
	hideDir(filepath.Join(r.absPath, InternalDirName))
	return r.r.OpenRoot(InternalDirName)
}

// RemoveInternal deletes the internal temp directory and its contents.
func (r *Root) RemoveInternal() error {
	return r.r.RemoveAll(InternalDirName)
}

// RenameFromInternal atomically moves a finished upload from the internal
// directory to dst (a cleaned guest path). It never overwrites: if dst exists
// ErrExists is returned.
func (r *Root) RenameFromInternal(tmpName, dst string) error {
	if _, err := r.r.Lstat(osPath(dst)); err == nil {
		return ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return mapErr(err)
	}
	return mapErr(r.r.Rename(InternalDirName+string(filepath.Separator)+tmpName, osPath(dst)))
}

// Exists reports whether a cleaned guest path exists (without following a
// final symlink).
func (r *Root) Exists(p string) bool {
	_, err := r.r.Lstat(osPath(p))
	return err == nil
}

// FreeSpace reports free bytes on the volume that holds the share.
func (r *Root) FreeSpace() (uint64, error) { return diskFree(r.absPath) }
