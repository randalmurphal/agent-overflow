// Package keyfile keeps one secret alone in an owner-only file: an API key
// or token the user pastes in, read fresh for each use.
//
// A File pairs the file's path with the Label that names it in every
// message a user reads. Read returns the secret as a secret.Bytes the caller
// zeroes, Inspect reports the file's state without the secret, Create makes
// the file empty for the user to fill, and Remove deletes it. An editor that
// saves by replacing the file can loosen its mode, so every open puts the
// file back to 0600 and its folder to 0700; a file AO cannot vouch for (a
// symlink, another user's, not a regular file) is refused, never followed.
//
// Where a file lives and what it is called belong to its owner
// (internal/openaikey, the tracker connections); this package owns how it
// is opened, checked and read.
package keyfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/secret"
)

const (
	// DirName is the owner-only folder under the app root that holds every
	// key file.
	DirName = "credentials"

	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	// maxFileBytes bounds the read. API keys and tokens are under 200 bytes.
	maxFileBytes = 4096
)

// Kind is what a service calls its secret.
type Kind string

const (
	// APIKey is a secret the service calls an API key.
	APIKey Kind = "key"
	// APIToken is a secret the service calls an API token.
	APIToken Kind = "token"
)

// Label names a key file in the messages a user reads: the file holding
// Label{Service: "OpenAI", Kind: APIKey} is "the OpenAI API key file".
type Label struct {
	// Service is whose secret the file holds.
	Service string
	// Kind is what the service calls it.
	Kind Kind
}

// secretName is the secret as messages name it: "OpenAI API key".
func (l Label) secretName() string {
	return l.Service + " API " + string(l.Kind)
}

func (l Label) valid() bool {
	return strings.TrimSpace(l.Service) == l.Service && l.Service != "" && (l.Kind == APIKey || l.Kind == APIToken)
}

// ErrMissing matches every error a File returns when there is no key file,
// or no folder for it.
var ErrMissing = errors.New("there is no key file")

// ErrEmpty matches every error a File returns when the key file holds no
// secret.
var ErrEmpty = errors.New("the key file is empty")

// ErrMissing returns the error a File with this label returns when there is
// no key file. It matches the File's error with errors.Is, and so does
// ErrMissing; another label's does not.
func (l Label) ErrMissing() error { return labeledError{label: l, err: ErrMissing} }

// ErrEmpty returns the error a File with this label returns when the key
// file is empty. It matches the File's error with errors.Is, and so does
// ErrEmpty; another label's does not.
func (l Label) ErrEmpty() error { return labeledError{label: l, err: ErrEmpty} }

// labeledError is ErrMissing or ErrEmpty worded for one label. It is a
// comparable value, so errors.Is matches two of them by label and cause.
type labeledError struct {
	label Label
	err   error
}

func (e labeledError) Error() string {
	if e.err == ErrEmpty {
		return "the " + e.label.secretName() + " file is empty"
	}
	return "there is no " + e.label.secretName() + " file"
}

func (e labeledError) Unwrap() error { return e.err }

// RefusedError means the file is there but AO will not use it. Only this
// package makes one.
type RefusedError struct {
	// Reason says why, without the file's contents.
	Reason string
	label  Label
}

func (e *RefusedError) Error() string {
	return "Agent Overflow will not use the " + e.label.secretName() + " file: " + e.Reason
}

// File is one key file. Make it with New; every message it returns names
// the file by its label.
type File struct {
	path  string
	label Label
}

// New describes the key file at path. It panics when the label has no
// service, or a service with surrounding space, or a Kind other than APIKey
// or APIToken: a label is fixed in code, and a message must never name the
// file wrongly.
func New(path string, label Label) File {
	if !label.valid() {
		panic(fmt.Sprintf("keyfile: invalid label %+v", label))
	}
	return File{path: path, label: label}
}

// Path is the key file's path.
func (f File) Path() string { return f.path }

// made panics for a File not made by New, which would word its messages
// without a label.
func (f File) made() {
	if !f.label.valid() {
		panic("keyfile: File used without New")
	}
}

func (f File) refused(reason string) error {
	return &RefusedError{Reason: reason, label: f.label}
}

// failed wraps a failed step: "open the OpenAI API key file: ...".
func (f File) failed(step, what string, err error) error {
	return fmt.Errorf("%s the %s %s: %w", step, f.label.secretName(), what, err)
}

// closeInto closes the key file or its folder and joins a failure into
// *err, so it is reported rather than dropped.
func (f File) closeInto(err *error, close func() error, what string) {
	if closeErr := close(); closeErr != nil {
		*err = errors.Join(*err, f.failed("close", what, closeErr))
	}
}

// Read returns the secret for one use. The caller zeroes it when the use
// no longer needs it. The file must hold one token of printable ASCII,
// optionally surrounded by whitespace.
func (f File) Read() (key *secret.Bytes, err error) {
	f.made()
	file, err := f.openOwnerOnly()
	if err != nil {
		return nil, err
	}
	defer func() {
		f.closeInto(&err, file.Close, "file")
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
		return nil, f.failed("read", "file", err)
	}
	if n > maxFileBytes {
		return nil, f.refused("it is larger than an API " + string(f.label.Kind))
	}
	trimmed := bytes.TrimSpace(buf[:n])
	if len(trimmed) == 0 {
		return nil, f.label.ErrEmpty()
	}
	for _, c := range trimmed {
		if c <= ' ' || c > '~' {
			kind := string(f.label.Kind)
			return nil, f.refused("it holds more than one " + kind + "; leave only the " + kind + " in it")
		}
	}
	return secret.New(trimmed), nil
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

// Status describes the key file without the secret.
type Status struct {
	State State `json:"state"`
	// Problem says why a refused file is refused.
	Problem string `json:"problem,omitempty"`
}

// Inspect reports the key file's state. It reads the secret to check it
// and zeroes it before returning.
func (f File) Inspect() Status {
	key, err := f.Read()
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
// the user can paste the secret into it. It reports whether it made the
// file; an existing file is left as it is once it passes the same checks a
// read makes.
func (f File) Create() (bool, error) {
	f.made()
	if err := os.MkdirAll(filepath.Dir(f.path), dirPerm); err != nil {
		return false, f.failed("create", "folder", err)
	}
	return f.createOwnerOnly()
}

// Remove deletes the key file. A file or folder that is not there is
// already removed. The folder is checked as a read checks it and is never
// followed through a link; a symlink at the file's own name is removed
// itself, and its target is left alone.
func (f File) Remove() error {
	f.made()
	return f.removeOwnerOnly()
}
