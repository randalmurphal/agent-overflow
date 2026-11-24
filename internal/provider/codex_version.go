package provider

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

// MinimumCodexCLIVersion is the oldest Codex app-server AO talks to: every
// client that completes `initialize` refuses an older one
// (codex.admitInitialize), and detection reports it. One-shot `codex exec`
// text generation (textgen) is not checked against it. AO shares one
// app-server per account and launches it with `thread_unload_delay_secs=0`
// (codex/launch.go) so a thread unloads the moment its last subscriber
// leaves; the setting first ships in rust-v0.154.0. An older app-server
// ignores the unknown key and keeps the thread loaded for 30 minutes, so
// every close that waits for `thread/closed` times out. Pre-releases of
// 0.154.0 order below it and are refused.
const MinimumCodexCLIVersion = "0.154.0"

// ErrCodexVersionUnsupported is the sentinel every *CodexVersionError
// unwraps to: the Codex binary reported a version below
// MinimumCodexCLIVersion. It is final until the binary changes.
var ErrCodexVersionUnsupported = errors.New("codex version unsupported")

// codexVersionTextLimit bounds the version text a refusal keeps and quotes
// back to the user.
const codexVersionTextLimit = 120

// CodexVersionError is a refusal of one Codex binary: Version is the
// version it reported, below the minimum, and Path the binary that was
// checked. CheckCodexVersion builds it with Version copied and capped at
// codexVersionTextLimit.
type CodexVersionError struct {
	Path    string
	Version string
}

func (e *CodexVersionError) Error() string {
	at := ""
	if e.Path != "" {
		at = " (" + e.Path + ")"
	}
	return fmt.Sprintf(
		"Codex v%s%s is installed; Agent Overflow needs v%s or newer. Update Codex.",
		e.Version, at, MinimumCodexCLIVersion,
	)
}

func (e *CodexVersionError) Unwrap() error { return ErrCodexVersionUnsupported }

// CodexVersionUnsupportedCode is the wire code a refused Codex answers a
// call with.
const CodexVersionUnsupportedCode = "codex_version_unsupported"

// PublicDetails makes the refusal the caller's answer as written, without
// the wrapping each caller adds (errorsx.Detailed).
func (e *CodexVersionError) PublicDetails() (code, message string) {
	return CodexVersionUnsupportedCode, e.Error()
}

// CheckCodexVersion checks the version a Codex binary reported against the
// minimum. version is a build ParseCodexVersion accepts and path the binary
// it came from. It returns nil for a supported version and a
// *CodexVersionError for an older one; text that is not a version is a
// caller's error, never a refusal, because a reply that names no build is
// not read (codex.admitInitialize, DetectProvider).
func CheckCodexVersion(path, version string) error {
	if _, ok := ParseCodexVersion(version); !ok {
		return fmt.Errorf("check the Codex version of %s: %q is not a version", path, codexVersionText(version))
	}
	if CompareCodexVersions(version, MinimumCodexCLIVersion) < 0 {
		return &CodexVersionError{Path: path, Version: codexVersionText(version)}
	}
	return nil
}

// codexVersionText is version text as a refusal keeps it: trimmed, capped
// and copied, so a cached or wrapped refusal never pins the buffer it came
// from.
func codexVersionText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > codexVersionTextLimit {
		cut := codexVersionTextLimit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		return strings.Clone(text[:cut]) + "..."
	}
	return strings.Clone(text)
}

// ParseCodexVersion is the one grammar of a Codex build version, the
// CARGO_PKG_VERSION a Codex build reports: strict SemVer 2.0.0,
// MAJOR.MINOR.PATCH with an optional -prerelease and an optional +build,
// numbers without leading zeros and no empty identifier. It returns an
// owned copy of the whole token, or false. Both readers of a build use it:
// the `initialize` user agent (codex.admitInitialize) and `codex --version`
// (ParseCodexCLIVersion), so they cannot disagree about one build.
func ParseCodexVersion(token string) (string, bool) {
	core, _, _ := strings.Cut(token, "+")
	core, _, _ = strings.Cut(core, "-")
	// semver accepts the shorthands vMAJOR and vMAJOR.MINOR; a build is
	// always three numbers.
	if strings.Count(core, ".") != 2 || !semver.IsValid("v"+token) {
		return "", false
	}
	return strings.Clone(token), true
}

// CompareCodexVersions orders two versions ParseCodexVersion accepts by
// SemVer precedence: a prerelease orders below its release and build
// metadata is ignored. Negative when left is older, zero when they are
// equal, positive when left is newer.
func CompareCodexVersions(left, right string) int {
	return semver.Compare("v"+left, "v"+right)
}

// ParseCodexCLIVersion reads the build from `codex --version` output
// (`codex-cli 0.154.0`): the first whitespace-separated word, stripped of
// surrounding brackets and punctuation, that is a version. It also reads the
// display forms a wrapper may print, `v0.154.0` and the two-part `0.154`,
// normalizing each to the build it names before ParseCodexVersion judges
// it. Returns "" when no word is a version, which callers treat as unknown,
// never as old or new. The `initialize` user agent is read by
// codex.admitInitialize instead.
func ParseCodexCLIVersion(output string) string {
	for _, word := range strings.Fields(output) {
		word = strings.Trim(word, "()[]{},;:'\"")
		if version, ok := ParseCodexVersion(normalizeCodexDisplayVersion(word)); ok {
			return version
		}
	}
	return ""
}

// normalizeCodexDisplayVersion turns a display spelling of a build into the
// build: a leading v is dropped and a missing patch is 0. Anything else is
// returned as it is, for ParseCodexVersion to judge.
func normalizeCodexDisplayVersion(word string) string {
	word = strings.TrimPrefix(word, "v")
	end := strings.IndexAny(word, "-+")
	if end < 0 {
		end = len(word)
	}
	if strings.Count(word[:end], ".") == 1 {
		return word[:end] + ".0" + word[end:]
	}
	return word
}
