//go:build !linux

package browser

import "os"

// linkFile is the link a placement makes (linkDownload).
var linkFile = os.Link

// placeHeld has no held source to link here: placeNoReplace places by
// name (errNoHeldLink).
func placeHeld(placeSource, placeDir, string, func(i int) string) (string, os.FileInfo, error) {
	return "", nil, errNoHeldLink
}
