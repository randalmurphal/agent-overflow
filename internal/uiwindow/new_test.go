package uiwindow

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestShellOptionsDisableUnusedWindowEventJavaScript(t *testing.T) {
	options := application.WebviewWindowOptions{
		Title: "client shell", URL: "http://127.0.0.1:1234/", Width: 1280, Height: 800,
	}
	configured := shellOptions(options)
	if !configured.DisableWindowEventForwarding {
		t.Fatal("shell window events must not inject JavaScript that clears WebKit user activation")
	}
	if configured.Title != options.Title || configured.URL != options.URL || configured.Width != options.Width || configured.Height != options.Height {
		t.Fatal("shell configuration changed caller-owned window options")
	}
}
