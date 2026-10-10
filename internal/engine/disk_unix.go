//go:build !windows

package engine

import "golang.org/x/sys/unix"

// freeBytes is the free space for this user on the file system of dir (an existing folder).
func freeBytes(dir string) int64 {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
