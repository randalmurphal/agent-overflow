package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// MaxProfileBytes bounds profile reads before YAML decoding.
const MaxProfileBytes int64 = 1 << 20

// Parse decodes exactly one profile document with strict field checking.
func Parse(r io.Reader) (Profile, error) {
	profile := Default()
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("decode profile: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Profile{}, fmt.Errorf("decode profile: multiple YAML documents are not allowed")
		}
		return Profile{}, fmt.Errorf("decode profile trailing document: %w", err)
	}
	return profile, nil
}

// ParseBytes decodes one strict profile document.
func ParseBytes(data []byte) (Profile, error) { return Parse(bytes.NewReader(data)) }

// Load reads, strictly parses, and validates path. When path does not exist it
// returns Default(), true, nil. Other filesystem and profile errors are fatal.
func Load(path string) (loaded Profile, defaulted bool, err error) {
	data, err := readLimitedFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), true, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	loaded, err = ParseBytes(data)
	if err != nil {
		return Profile{}, false, fmt.Errorf("parse profile %q: %w", path, err)
	}
	result := Validate(loaded)
	if !result.Valid() {
		return Profile{}, false, &ValidationError{Findings: result.Findings}
	}
	return loaded, false, nil
}

func readLimitedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open profile %q: %w", path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxProfileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read profile %q: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close profile %q: %w", path, closeErr)
	}
	if int64(len(data)) > MaxProfileBytes {
		return nil, fmt.Errorf("profile %q exceeds %d-byte limit", path, MaxProfileBytes)
	}
	return data, nil
}
