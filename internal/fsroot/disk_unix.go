//go:build !windows

package fsroot

import "golang.org/x/sys/unix"

func diskFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// hideDir is a no-op on Unix: the leading dot already hides the folder.
func hideDir(string) {}
