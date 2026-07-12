package profile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// MaxSecretFileBytes bounds a resolved file value and excludes files that can
// grow without end while being read.
const MaxSecretFileBytes int64 = 1 << 20

// ResolvedSecrets carries ephemeral secret values and every value that must be
// registered for masking. It deliberately has no value-bearing String method.
type ResolvedSecrets struct {
	Values map[string]string `json:"-"`
	Masks  []string          `json:"-"`
}

// String reports only cardinality so generic string formatting cannot expose
// resolved values.
func (r ResolvedSecrets) String() string {
	return fmt.Sprintf("ResolvedSecrets{count:%d}", len(r.Values))
}

// GoString applies the same redaction to Go-syntax diagnostic formatting.
func (r ResolvedSecrets) GoString() string { return r.String() }

// SecretResolutionError identifies a failed reference without exposing a value.
type SecretResolutionError struct {
	Secret string
	Source string
	Err    error
}

func (e *SecretResolutionError) Error() string {
	return fmt.Sprintf("resolve secret %q from %s: %v", e.Secret, e.Source, e.Err)
}

func (e *SecretResolutionError) Unwrap() error { return e.Err }

// ResolveSecrets explicitly resolves all declared env and file references.
// Load and Parse never call this method.
func (p Profile) ResolveSecrets() (ResolvedSecrets, error) {
	resolved := ResolvedSecrets{
		Values: make(map[string]string, len(p.Secrets)),
		Masks:  make([]string, 0, len(p.Secrets)),
	}
	names := make([]string, 0, len(p.Secrets))
	for name := range p.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		reference := p.Secrets[name]
		var value string
		switch reference.Source {
		case "env":
			resolvedValue, ok := os.LookupEnv(reference.Env)
			if !ok {
				return ResolvedSecrets{}, &SecretResolutionError{Secret: name, Source: "env", Err: fmt.Errorf("environment variable %q is not set", reference.Env)}
			}
			value = resolvedValue
		case "file":
			data, err := readSecretFile(reference.Path)
			if err != nil {
				return ResolvedSecrets{}, &SecretResolutionError{Secret: name, Source: "file", Err: fmt.Errorf("read referenced file %q: %w", reference.Path, err)}
			}
			// Secret files conventionally end with a trailing newline the
			// value itself does not contain. Keeping it would poison the
			// mask list: "abc\n" never matches the "abc" that appears in
			// narrative text, so the D2-8 masking pass would miss it.
			value = strings.TrimRight(string(data), "\r\n")
		default:
			return ResolvedSecrets{}, &SecretResolutionError{Secret: name, Source: reference.Source, Err: fmt.Errorf("unsupported source")}
		}
		resolved.Values[name] = value
		resolved.Masks = append(resolved.Masks, value)
	}
	return resolved, nil
}

func readSecretFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("inspect: %w", statErr), closeSecretFile(file))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("must be a regular file"), closeSecretFile(file))
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxSecretFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close: %w", closeErr)
	}
	if int64(len(data)) > MaxSecretFileBytes {
		return nil, fmt.Errorf("exceeds %d-byte limit", MaxSecretFileBytes)
	}
	return data, nil
}

func closeSecretFile(file *os.File) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}
