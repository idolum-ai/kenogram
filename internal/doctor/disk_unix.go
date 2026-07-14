//go:build linux || darwin

package doctor

import "syscall"

func diskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func directoryAccess(path string) error {
	const writeAndTraverse = 2 | 1 // access(2): W_OK | X_OK
	return syscall.Access(path, writeAndTraverse)
}
