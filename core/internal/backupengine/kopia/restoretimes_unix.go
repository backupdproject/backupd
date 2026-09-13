//go:build unix

package kopia

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lchtimes sets a symbolic link's own modification time, without
// following it.
//
// It exists because the standard library has no Lchtimes: os.Chtimes
// follows a link, so using it on a restored link would rewrite the
// timestamp of whatever the link points AT -- a write outside the restore
// destination, which is the one thing this package's restore path refuses
// everywhere. utimensat with AT_SYMLINK_NOFOLLOW is the syscall that does
// the intended thing, and it is available on every GOOS the "unix"
// pseudo-tag covers, including the linux/arm64 cross-compile targets and
// darwin for local development.
//
// The atime is set to the same instant as the mtime, which is what the
// rest of this restore does for files and directories: a restore has no
// meaningful access time to reproduce, and leaving it to be "the moment
// this link was created" would make a restored tree's atimes the one
// piece of metadata that is about the restore instead of the data.
func lchtimes(path string, mod time.Time) error {
	times := []unix.Timespec{
		unix.NsecToTimespec(mod.UnixNano()),
		unix.NsecToTimespec(mod.UnixNano()),
	}

	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("setting the times of the symbolic link %s: %w", path, os.NewSyscallError("utimensat", err))
	}

	return nil
}
