package main

import (
	"context"

	"agent-overflow/internal/externalurl"
)

// OpenExternalURL opens an absolute HTTP(S) URL in the user's system
// browser. The backend owns this instead of letting the webview handle
// target=_blank / window.open so WSL can deliberately cross into the
// Windows default browser instead of launching a Linux/WSLg browser.
func (a *App) OpenExternalURL(rawURL string) error {
	return externalurl.Open(context.Background(), rawURL)
}
