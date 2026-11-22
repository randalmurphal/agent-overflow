// Package openaikey names the OpenAI API key file a user keeps for Agent
// Overflow's API voice and live dictation.
//
// The key lives in its own file, credentials/openai-api-key under the app
// root, owner-only like the providers' credential files. Nothing else holds
// it: Read returns it fresh for one request as a secret.Bytes the caller
// zeroes, and Inspect reports the file's state without the key. How the
// file is opened, checked and read belongs to internal/keyfile; this
// package owns the file's name, its place and how messages name it.
package openaikey

import (
	"path/filepath"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/secret"
)

const (
	// DirName is the owner-only folder under the app root that holds the
	// key file.
	DirName = keyfile.DirName
	// FileName is the key file's name.
	FileName = "openai-api-key"
)

// label names the file in every message: "the OpenAI API key file".
var label = keyfile.Label{Service: "OpenAI", Kind: keyfile.APIKey}

// Path is the key file under the app root.
func Path(appRoot string) string {
	return filepath.Join(appRoot, DirName, FileName)
}

// ErrMissing means there is no key file, or no folder for it. Errors that
// match it also match keyfile.ErrMissing.
var ErrMissing = label.ErrMissing()

// ErrEmpty means the key file holds no key. Errors that match it also
// match keyfile.ErrEmpty.
var ErrEmpty = label.ErrEmpty()

// RefusedError means the file is there but AO will not use it.
type RefusedError = keyfile.RefusedError

// Read returns the key for one request. The caller zeroes it when the
// request no longer needs it.
func Read(path string) (*secret.Bytes, error) {
	return keyfile.New(path, label).Read()
}

// State is what Inspect found.
type State string

const (
	StatePresent = State(keyfile.StatePresent)
	StateMissing = State(keyfile.StateMissing)
	StateEmpty   = State(keyfile.StateEmpty)
	// StateRefused covers a file AO will not use and one it cannot read;
	// Status.Problem says which.
	StateRefused = State(keyfile.StateRefused)
)

// Status describes the key file without the key.
type Status struct {
	State State `json:"state"`
	// Problem says why a refused file is refused.
	Problem string `json:"problem,omitempty"`
}

// Inspect reports the key file's state. It reads the key to check it and
// zeroes it before returning.
func Inspect(path string) Status {
	status := keyfile.New(path, label).Inspect()
	return Status{State: State(status.State), Problem: status.Problem}
}

// Create makes the key file, empty and owner-only, when it is missing, so
// the user can paste a key into it. It reports whether it made the file;
// an existing file is left as it is once it passes the same checks a read
// makes.
func Create(path string) (bool, error) {
	return keyfile.New(path, label).Create()
}
