//go:build darwin || linux

package atomicfile

import "errors"

func exclusiveRenameUnsupported(err error) bool {
	return errors.Is(err, exclusiveRenameUnsupportedErrno)
}
