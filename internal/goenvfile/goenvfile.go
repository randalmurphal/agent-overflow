// Package goenvfile resolves a Go setting as the go command does, from the
// environment and the Go environment file `go env -w` writes, without
// starting it. Standard library only.
//
// It follows cmd/go/internal/cfg (EnvFile, readEnvFile, Getenv) and
// cmd/go/internal/cache (DefaultDir) of the toolchain this repository
// uses. It does not read $GOROOT/go.env, which the go command reads after
// the user's file for keys that file leaves unset; the toolchains Go
// distributes set only GOPROXY, GOSUMDB and GOTOOLCHAIN there.
package goenvfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Read returns the file the go command would read now (GOENV, else go/env
// in the user config directory), or nothing when there is none or GOENV is
// off. A file that exists but cannot be read is an error, where the go
// command would ignore it, so a caller never acts on a guess.
func Read() ([]byte, error) {
	path := os.Getenv("GOENV")
	if path == "off" {
		return nil, nil
	}
	if path == "" {
		config, err := os.UserConfigDir()
		if err != nil || config == "" {
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
// when it gives none. It parses as the go command does: lines end at '\n';
// a line without '=' or whose first byte is not 'A' to 'Z' is ignored; the
// key is every byte before the first '=' and the value every byte after
// it, spaces and '\r' included; the last line for a key wins.
func Lookup(data []byte, name string) string {
	value := ""
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		i := bytes.IndexByte(line, '=')
		if i < 0 || line[0] < 'A' || 'Z' < line[0] {
			continue
		}
		if string(line[:i]) == name {
			value = string(line[i+1:])
		}
	}
	return value
}

// Getenv returns the go command's value for name: the environment's when
// it is not empty, else the Go environment file's.
func Getenv(name string) (string, error) {
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	data, err := Read()
	if err != nil {
		return "", err
	}
	return Lookup(data, name), nil
}

// CacheDir returns the build cache directory the go command uses, the one
// `go env GOCACHE` names. Where the go command has none, GOCACHE=off, a
// relative GOCACHE or no user cache directory, it returns an error.
func CacheDir() (string, error) {
	dir, err := Getenv("GOCACHE")
	if err != nil {
		return "", err
	}
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("GOCACHE is not set and %w", err)
		}
		return filepath.Join(cache, "go-build"), nil
	}
	if dir == "off" {
		return "", errors.New("the Go build cache is off (GOCACHE=off)")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("GOCACHE %q is not an absolute path, so the go command has no build cache", dir)
	}
	return dir, nil
}
