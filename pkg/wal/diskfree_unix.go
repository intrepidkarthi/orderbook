//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package wal

import "syscall"

func freeBytes(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
