package atomicfile

import "golang.org/x/sys/windows"

func renameNoReplace(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// No REPLACE_EXISTING and no COPY_ALLOWED: publication cannot overwrite
	// another owner or silently become a non-atomic cross-volume copy.
	return windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH)
}
