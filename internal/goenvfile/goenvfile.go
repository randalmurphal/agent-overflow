// Package goenvfile reads the Go environment file that `go env -w` writes,
// so a tool can resolve a Go setting as the go command would without
// starting it. Standard library only.
package goenvfile

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Read returns the file the go command would read now (GOENV, else go/env
// in the user config directory), or nothing when there is none or GOENV is
// off.
func Read() ([]byte, error) {
	path := os.Getenv("GOENV")
	if path == "off" {
		return nil, nil
	}
	if path == "" {
		config, err := os.UserConfigDir()
		if err != nil {
			return nil, nil // the go command then reads no file either
		}
		path = filepath.Join(config, "go", "env")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the Go environment file: %w", err)
	}
	return data, nil
}

// Lookup returns the value data, a Go environment file, gives name, or ""
// when it gives none.
func Lookup(data []byte, name string) string {
	value := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, v, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if ok && strings.TrimSpace(key) == name {
			value = strings.TrimSpace(v)
		}
	}
	return value
}
