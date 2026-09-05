//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly || solaris

package vfs

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func fileChtimes(f *os.File, a, m time.Time) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	err = raw.Control(func(fd uintptr) {
		callErr = unix.Futimes(int(fd), []unix.Timeval{unix.NsecToTimeval(a.UnixNano()), unix.NsecToTimeval(m.UnixNano())})
	})
	if err != nil {
		return err
	}
	return callErr
}
