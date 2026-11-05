package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Open each component without following links before making any filesystem
// change. Keep custody of the final directory by descriptor for every later IO.
// Parents must exist; existing directories are never chmodded.
func openPrivateStateDir(path string) (*os.File, error) {
	for _, component := range strings.Split(path, string(os.PathSeparator)) {
		if component == ".." {
			return nil, fmt.Errorf("state directory cannot contain parent traversal")
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	current, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	components := strings.Split(strings.TrimPrefix(absolute, "/"), "/")
	for i, name := range components {
		if name == "" {
			current.Close()
			return nil, fmt.Errorf("invalid state directory")
		}
		fd, err := syscall.Openat(int(current.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err == syscall.ENOENT && i == len(components)-1 {
			if err = syscall.Mkdirat(int(current.Fd()), name, 0700); err != nil {
				current.Close()
				return nil, err
			}
			if err = current.Sync(); err != nil {
				current.Close()
				return nil, err
			}
			fd, err = syscall.Openat(int(current.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		current.Close()
		if err != nil {
			return nil, fmt.Errorf("unsafe state directory: %w", err)
		}
		current = os.NewFile(uintptr(fd), name)
	}
	info, err := current.Stat()
	if err != nil {
		current.Close()
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		current.Close()
		return nil, fmt.Errorf("state directory must be private and owned")
	}
	return current, nil
}
func openStateFile(dir *os.File, name string, flags int) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("state file must be regular")
	}
	return f, nil
}
