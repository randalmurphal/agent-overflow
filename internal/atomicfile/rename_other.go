//go:build !darwin && !linux && !windows

package atomicfile

import "errors"

func platformExclusiveRename(string, string) error {
	return errors.New("atomic publication is unsupported on this platform")
}

func exclusiveRenameUnsupported(error) bool { return false }
