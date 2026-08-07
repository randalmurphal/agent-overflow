package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// ChannelInstall is the transport event channel the backend emits an
	// InstallDirective on and the Windows launcher subscribes to.
	ChannelInstall = "updater:install"

	// StagingDirName is the directory, relative to the app-managed root on each
	// side of the WSL boundary, that holds downloaded-but-not-yet-installed
	// artifacts: `%APPDATA%\agent-overflow\update` for the launcher, the same
	// directory seen through /mnt/c as `<wsldistro.WSLConfigDir()>/update` for
	// the backend.
	StagingDirName = "update"

	// MarkerFileName is the Marker file inside the backend's data dir.
	MarkerFileName = "update-intent.json"

	// maxFilenameLen bounds a staged artifact's file name. Release asset names
	// are well under this; the cap keeps a hostile directive from producing an
	// unwieldy path on the Windows side.
	maxFilenameLen = 128

	// maxVersionLen bounds a release version string, mirroring the tag-body cap
	// the updater's release-tag validation uses.
	maxVersionLen = 64
)

// stagedExeNamePattern constrains a staged artifact's file name to the shape our
// release pipeline publishes: an .exe whose name is alphanumeric plus dot, dash
// and underscore. It is anchored, so anything with a separator, a drive letter,
// a space or a control character is rejected outright.
var stagedExeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.exe$`)

// releaseVersionPattern constrains a release version. It is an independent copy
// of the shape the main package's release-tag validation enforces (this package
// must not import main), minus the optional leading "v" — directives carry the
// bare version.
var releaseVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.\-_+]*$`)

// InstallDirective is the wire payload the backend emits on ChannelInstall once
// it has staged a verified release, and the launcher consumes to drive its own
// swap. Filename is a BARE file name — never a path — that the launcher resolves
// strictly under its own staging directory, so a directive can never name a file
// outside it.
type InstallDirective struct {
	Filename string `json:"filename"` // e.g. "agent-overflow-wsl-amd64.exe"
	SHA256   string `json:"sha256"`   // 64 hex chars — the release digest from SHASUMS256
	Version  string `json:"version"`  // release version, no "v" prefix
}

// Validate reports the first thing wrong with d, or nil when every field is
// safe to act on. It is the launcher's gate before touching the filesystem.
func (d InstallDirective) Validate() error {
	if err := validateBareFilename(d.Filename); err != nil {
		return err
	}
	if !stagedExeNamePattern.MatchString(d.Filename) {
		return fmt.Errorf("selfupdate: filename %q is not a staged .exe artifact name", d.Filename)
	}
	if _, err := decodeSHA256(d.SHA256); err != nil {
		return err
	}
	return validateVersion(d.Version)
}

// Digest returns the decoded 32-byte SHA-256 the directive carries.
func (d InstallDirective) Digest() ([]byte, error) {
	return decodeSHA256(d.SHA256)
}

// validateBareFilename enforces that name is a single path component usable on
// both sides of the WSL boundary. filepath.Base alone is not enough: it is
// OS-dependent, so a backslash-separated Windows path survives it unchanged on
// Linux, which is exactly where these directives are produced.
func validateBareFilename(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("selfupdate: filename is empty")
	case len(name) > maxFilenameLen:
		return fmt.Errorf("selfupdate: filename is %d bytes, over the %d-byte limit", len(name), maxFilenameLen)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("selfupdate: filename %q contains a path separator", name)
	case name == "." || name == ".." || strings.Contains(name, ".."):
		return fmt.Errorf("selfupdate: filename %q contains a parent-directory reference", name)
	case filepath.Base(name) != name:
		return fmt.Errorf("selfupdate: filename %q is not a bare file name", name)
	}
	return nil
}

func validateVersion(version string) error {
	switch {
	case version == "":
		return fmt.Errorf("selfupdate: version is empty")
	case len(version) > maxVersionLen:
		return fmt.Errorf("selfupdate: version is %d bytes, over the %d-byte limit", len(version), maxVersionLen)
	case !releaseVersionPattern.MatchString(version):
		return fmt.Errorf("selfupdate: version %q is not a valid release version", version)
	}
	return nil
}

// decodeSHA256 decodes a hex SHA-256 and enforces its length, so callers can
// never hand a short or garbled digest to the verifier.
func decodeSHA256(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("selfupdate: sha256 is empty")
	}
	if len(s) != hex.EncodedLen(sha256.Size) {
		return nil, fmt.Errorf("selfupdate: sha256 is %d chars, want %d", len(s), hex.EncodedLen(sha256.Size))
	}
	digest, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: sha256 %q is not hex: %w", s, err)
	}
	return digest, nil
}

// validateDigest enforces that a raw digest is a SHA-256 the verifier can use.
func validateDigest(digest []byte) error {
	if len(digest) != sha256.Size {
		return fmt.Errorf("selfupdate: digest is %d bytes, want %d", len(digest), sha256.Size)
	}
	return nil
}
