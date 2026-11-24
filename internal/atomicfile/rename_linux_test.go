package atomicfile

import (
	"testing"

	"golang.org/x/sys/unix"
)

// renameat2 answers EINVAL on a filesystem without RENAME_NOREPLACE (NFS,
// 9p), EOPNOTSUPP on one that says so, and ENOSYS without the call. Each
// falls back; a refusal of the destination or the device does not.
func TestExclusiveRenameUnsupportedCoversLinuxErrnos(t *testing.T) {
	t.Parallel()
	for errno, want := range map[unix.Errno]bool{
		unix.EINVAL: true, unix.EOPNOTSUPP: true, unix.ENOSYS: true,
		unix.EEXIST: false, unix.EXDEV: false, unix.EACCES: false,
	} {
		if got := exclusiveRenameUnsupported(errno); got != want {
			t.Errorf("exclusiveRenameUnsupported(%v) = %v, want %v", errno, got, want)
		}
	}
}
