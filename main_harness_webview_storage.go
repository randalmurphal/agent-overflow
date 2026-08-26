package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// Webview storage isolation for windowed isolated boots.
//
// The provider pins in newIsolatedProviderApp make the BACKEND
// harness-local. They say nothing about the webview: WebKitGTK's
// default network session puts cookies, localStorage, IndexedDB (the
// thread replica) and shader caches under the XDG base directories, so
// two windowed instances — or one instance and the developer's real app
// — would share them, and a harness reset would leave a foreign replica
// behind to be re-adopted on the next boot.
//
// Spike 2026-08-26 verified that pointing the three XDG variables at
// the data root moves every one of those under it.

// xdgVar is one environment variable an isolated windowed boot owns.
type xdgVar struct {
	Name string
	Dir  string
}

// webviewStorageEnv derives the XDG environment for a data root. Pure,
// so the naming rule is testable on any platform without a display
// server or a GLib in the process.
//
// It lands under <dataRoot>/home/xdg — beside the redirected HOME
// rather than inside it, so a fixture that seeds ~/.claude and a
// webview that writes its caches never walk over each other, and a
// `rm -rf <dataRoot>` still takes both.
func webviewStorageEnv(dataRoot string) []xdgVar {
	base := filepath.Join(dataRoot, "home", "xdg")
	// Ordered, not a map: the boot log prints these and a stable order
	// makes two boots diffable.
	return []xdgVar{
		{Name: "XDG_CACHE_HOME", Dir: filepath.Join(base, "cache")},
		{Name: "XDG_CONFIG_HOME", Dir: filepath.Join(base, "config")},
		{Name: "XDG_DATA_HOME", Dir: filepath.Join(base, "data")},
	}
}

// isolateWebviewStorage points this process's XDG base directories at
// the data root, creating them 0700.
//
// Ordering is load-bearing at both ends. It must run AFTER
// prepareHarness, whose data-dir refusals compare the requested root
// against the REAL os.UserConfigDir() — moving XDG_CONFIG_HOME first
// would make "is this the user's config root" answer about our own
// scratch directory and disarm the check. And it must run BEFORE the
// first Wails/GLib call, because GLib resolves and caches the base
// directories on first use; a setenv after that is ignored (verified by
// spike 2026-08-26).
//
// AO_HARNESS_KEEP_HOME deliberately does not opt out. That flag widens
// what the harness may READ from the developer's provider homes; webview
// storage is never shared, in either direction.
//
// Failures are loud, not fatal: an instance whose webview storage fell
// back to the user's XDG dirs still runs, and the log line is what tells
// an operator why two instances started fighting over localStorage.
func isolateWebviewStorage(dataRoot string) {
	switch runtime.GOOS {
	case "linux":
	case "darwin":
		// WKWebView's default data store is keyed by bundle identity, not
		// by $HOME, so a dev binary shares storage across instances no
		// matter what we set here. Naming it beats a silent no-op: the
		// symptom (two windowed harnesses sharing localStorage) is
		// otherwise indistinguishable from a bug in the app.
		log.Printf("harness: macOS webview storage is bundle-keyed (WKWebView default data store); windowed instances on this host share cookies/localStorage/IndexedDB. See docs/specs/testing-harness.md §1.")
		return
	default:
		// Windows hosts its windowed instances through the launcher,
		// which already gives each profile its own WebView2 user-data dir.
		return
	}
	for _, v := range webviewStorageEnv(dataRoot) {
		if err := os.MkdirAll(v.Dir, 0o700); err != nil {
			log.Printf("harness: create %s at %s: %v (webview storage stays on the user's XDG dirs)", v.Name, v.Dir, err)
			continue
		}
		if err := os.Setenv(v.Name, v.Dir); err != nil {
			log.Printf("harness: set %s: %v (webview storage stays on the user's XDG dirs)", v.Name, err)
			continue
		}
		log.Printf("harness: %s=%s", v.Name, v.Dir)
	}
}
