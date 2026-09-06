//go:build !darwin && !linux && !windows

package atomicfile

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("atomic publication is unsupported on this platform")
}
