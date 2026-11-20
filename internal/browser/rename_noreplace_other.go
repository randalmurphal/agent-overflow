//go:build !linux && !darwin

package browser

// renameNoReplace has no implementation here: no engine of this platform
// stages downloads.
func renameNoReplace(string, string) error { return errNoReplaceRename }
