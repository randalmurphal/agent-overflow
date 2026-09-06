package main

import (
	"os"
	"path/filepath"

	"agent-overflow/internal/keybindings"
)

func main() {
	if err := os.WriteFile(filepath.FromSlash(keybindings.FrontendDefaultsRelPath), []byte(keybindings.FrontendDefaultsSource()), 0o644); err != nil {
		panic(err)
	}
}
