//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris

package vfs

import (
	"os"
	"syscall"
	"time"
)

func fileChtimes(*os.File, time.Time, time.Time) error { return syscall.ENOSYS }
