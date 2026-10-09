package app

import (
	"context"

	"agent-overflow/internal/dirbrowse"
	"agent-overflow/internal/externalurl"
)

// Version returns the build-stamped semantic version (e.g. "0.0.1") or
// "dev" for unstamped builds. The frontend's Settings footer reads
// this to display the current release. Read-only, no FS / process /
// settings touch — deliberately an observe scope rather than `host`, so a
// remote --connect client sees the backend's version too.
//
//ao:scope threads:read
//ao:route home
func (a *App) Version() string {
	return a.version
}

// OpenExternalURL opens an absolute HTTP(S) URL in the user's system
// browser. The backend owns this instead of letting the webview handle
// target=_blank / window.open so WSL can deliberately cross into the
// Windows default browser instead of launching a Linux/WSLg browser.
//
//ao:scope host
//ao:route home
func (a *App) OpenExternalURL(rawURL string) error {
	return externalurl.Open(context.Background(), rawURL)
}

// BrowseDirectory lists the contents of path for the project-picker
// UI. The full contract (path normalisation, ordering, .git-marker
// detection, EntryLimit truncation) lives in internal/dirbrowse. This reads
// the selected computer; it opens no native picker and works for paired clients.
//
//ao:scope files:read
//ao:route selected
func (a *App) BrowseDirectory(path string) (dirbrowse.Listing, error) {
	return dirbrowse.Browse(path)
}
