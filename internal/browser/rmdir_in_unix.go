//go:build linux || darwin

package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// rmdirIn removes the empty directory name from parent, held open, and
// nothing else: a file at the name, or a directory holding anything, stays
// and is the error. os.Root.Remove would unlink a file put at the name.
func rmdirIn(parent *os.Root, name string) (err error) {
	if name == "" || name == "." || name != filepath.Base(name) || !filepath.IsLocal(name) {
		return fmt.Errorf("browser: %q is not one name in a directory", name)
	}
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	conn, err := dir.SyscallConn()
	if err != nil {
		return err
	}
	var rmErr error
	if err := conn.Control(func(fd uintptr) { rmErr = unix.Unlinkat(int(fd), name, unix.AT_REMOVEDIR) }); err != nil {
		return err
	}
	if rmErr != nil {
		return &os.PathError{Op: "rmdir", Path: name, Err: rmErr}
	}
	return nil
}
