// Package openaikey reads the OpenAI API key a user keeps for Agent
// Overflow's API voice and live dictation.
//
// The key lives in its own file, credentials/openai-api-key under the app
// root, owner-only like the providers' credential files. Nothing else holds
// it: Read returns it fresh for one request as a secret.Bytes the caller
// zeroes, and Inspect reports the file's state without the key. An editor
// that saves by replacing the file can loosen its mode, so a read puts the
// file back to 0600 and its folder to 0700; a file AO cannot vouch for (a
// symlink, another user's, not a regular file) is refused, never followed.
package openaikey

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"agent-overflow/internal/secret"
)

const (
	// DirName is the owner-only folder under the app root that holds the
	// key file.
	DirName = "credentials"
	// FileName is the key file's name.
	FileName = "openai-api-key"

	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	// maxFileBytes bounds the read. An OpenAI key is under 200 bytes.
	maxFileBytes = 4096
)

// Path is the key file under the app root.
func Path(appRoot string) string {
	return filepath.Join(appRoot, DirName, FileName)
}

// ErrMissing means there is no key file, or no folder for it.
var ErrMissing = errors.New("there is no OpenAI API key file")

// ErrEmpty means the key file holds no key.
var ErrEmpty = errors.New("the OpenAI API key file is empty")

// RefusedError means the file is there but AO will not use it.
type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string {
	return "Agent Overflow will not use the OpenAI API key file: " + e.Reason
}

func refused(reason string) error { return &RefusedError{Reason: reason} }

// Read returns the key for one request. The caller zeroes it when the
// request no longer needs it.
func Read(path string) (key *secret.Bytes, err error) {
	file, err := openOwnerOnly(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeInto(&err, file.Close, "file")
		if err != nil && key != nil {
			key.Zero()
			key = nil
		}
	}()
	// One fixed buffer, so no growing read leaves partial copies behind.
	buf := make([]byte, maxFileBytes+1)
	defer clear(buf)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("read the OpenAI API key file: %w", err)
	}
	if n > maxFileBytes {
		return nil, refused("it is larger than an API key")
	}
	trimmed := bytes.TrimSpace(buf[:n])
	if len(trimmed) == 0 {
		return nil, ErrEmpty
	}
	for _, c := range trimmed {
		if c <= ' ' || c > '~' {
			return nil, refused("it holds more than one key; leave only the key in it")
		}
	}
	return secret.New(trimmed), nil
}

// closeInto closes a key file or folder and joins a failure into *err, so
// it is reported rather than dropped.
func closeInto(err *error, close func() error, what string) {
	if closeErr := close(); closeErr != nil {
		*err = errors.Join(*err, fmt.Errorf("close the OpenAI API key %s: %w", what, closeErr))
	}
}

// State is what Inspect found.
type State string

const (
	StatePresent State = "present"
	StateMissing State = "missing"
	StateEmpty   State = "empty"
	// StateRefused covers a file AO will not use and one it cannot read;
	// Status.Problem says which.
	StateRefused State = "refused"
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
	key, err := Read(path)
	var refusal *RefusedError
	switch {
	case err == nil:
		key.Zero()
		return Status{State: StatePresent}
	case errors.Is(err, ErrMissing):
		return Status{State: StateMissing}
	case errors.Is(err, ErrEmpty):
		return Status{State: StateEmpty}
	case errors.As(err, &refusal):
		return Status{State: StateRefused, Problem: refusal.Reason}
	default:
		return Status{State: StateRefused, Problem: err.Error()}
	}
}

// Create makes the key file, empty and owner-only, when it is missing, so
// the user can paste a key into it. It reports whether it made the file;
// an existing file is left as it is once it passes the same checks a read
// makes.
func Create(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return false, fmt.Errorf("create the OpenAI API key folder: %w", err)
	}
	return createOwnerOnly(path)
}
