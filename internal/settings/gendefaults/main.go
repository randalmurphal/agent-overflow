// Command gendefaults writes the frontend's settings-defaults module
// from internal/settings.DefaultSettings, so the two cannot drift.
//
// Run it through `go generate ./internal/settings`.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/settings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gendefaults:", err)
		os.Exit(1)
	}
}

func run() error {
	// go generate runs with the working directory set to the package
	// holding the directive, so the relative path is the same one the
	// checked-in test compares against.
	out := filepath.FromSlash(settings.FrontendDefaultsRelPath)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(settings.FrontendDefaultsSource()), 0o644)
}
