//go:build !linux && !darwin

package browser

import (
	"errors"
	"os"
)

// rmdirIn has no directory-only removal through a held parent here, where
// no Manager runs: nothing is removed.
func rmdirIn(*os.Root, string) error {
	return errors.New("browser: a directory is not removed through its held parent on this system")
}
