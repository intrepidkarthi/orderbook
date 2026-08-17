//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package wal

// freeBytes cannot be answered portably here and says so rather than guessing.
func freeBytes(dir string) (int64, bool) { return 0, false }
