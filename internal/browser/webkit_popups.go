//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"strconv"

	"agent-overflow/internal/browser/ids"
)

// pageHandle is the handle of a WebKit engine's page numbered n. A popup
// is numbered as it is held (holdPopup) and reported and adopted under its
// page's handle, so it has one handle before and after its adoption: a
// popup it opens while held names it as its opener, and the Manager finds
// that opener listed under it. The view keeps n, so a download it starts
// names the popup too.
func pageHandle(n uint64) ids.PageHandle {
	return ids.PageHandle("page-" + strconv.FormatUint(n, 10))
}
