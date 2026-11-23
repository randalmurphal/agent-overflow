package provider

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinimumCodexCLIVersion is the oldest Codex AO runs. AO shares one
// app-server per account and launches it with `thread_unload_delay_secs=0`
// (codex/launch.go) so a thread unloads the moment its last subscriber
// leaves; the setting first ships in rust-v0.154.0. An older app-server
// ignores the unknown key and keeps the thread loaded for 30 minutes, so
// every close that waits for `thread/closed` times out. Pre-releases of
// 0.154.0 order below it and are refused.
const MinimumCodexCLIVersion = "0.154.0"

// ErrCodexVersionUnsupported is the sentinel every *CodexVersionError
// unwraps to: the Codex binary is below MinimumCodexCLIVersion or its
// version could not be read. It is final until the binary changes.
var ErrCodexVersionUnsupported = errors.New("codex version unsupported")

// codexVersionRawLimit bounds the version text quoted back to the user.
const codexVersionRawLimit = 120

// CodexVersionError is a refusal of one Codex binary. Version is the
// parsed version, "" when none could be read; Raw is the text it was read
// from (`codex --version` output or the app-server's initialize user
// agent); Path is the binary that was checked.
type CodexVersionError struct {
	Path    string
	Version string
	Raw     string
}

func (e *CodexVersionError) Error() string {
	at := ""
	if e.Path != "" {
		at = " (" + e.Path + ")"
	}
	if e.Version != "" {
		return fmt.Sprintf(
			"Codex v%s%s is installed; Agent Overflow needs v%s or newer. Update Codex.",
			e.Version, at, MinimumCodexCLIVersion,
		)
	}
	raw := strings.TrimSpace(e.Raw)
	if raw == "" {
		return fmt.Sprintf(
			"Agent Overflow could not read the version of Codex%s. Update Codex to v%s or newer.",
			at, MinimumCodexCLIVersion,
		)
	}
	if len(raw) > codexVersionRawLimit {
		raw = raw[:codexVersionRawLimit] + "..."
	}
	return fmt.Sprintf(
		"Agent Overflow could not read the version of Codex%s from %q. Update Codex to v%s or newer.",
		at, raw, MinimumCodexCLIVersion,
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

// CheckCodexVersion admits a Codex binary. version is the parsed version
// ("" when unreadable), raw the text it came from and path the binary
// checked. It returns nil for a supported version and a *CodexVersionError
// otherwise; an unreadable version is unsupported.
func CheckCodexVersion(path, version, raw string) error {
	if _, ok := parseCodexSemver(normalizeCodexCLIVersion(version)); !ok {
		return &CodexVersionError{Path: path, Raw: raw}
	}
	if compareCodexCLIVersions(version, MinimumCodexCLIVersion) < 0 {
		return &CodexVersionError{Path: path, Version: version, Raw: raw}
	}
	return nil
}

var codexVersionPattern = regexp.MustCompile(`\bv?(\d+\.\d+(?:\.\d+)?(?:-[0-9A-Za-z.-]+)?)\b`)

type parsedCodexSemver struct {
	major      int
	minor      int
	patch      int
	prerelease []string
}

func parseCodexCLIVersion(output string) string {
	match := codexVersionPattern.FindStringSubmatch(output)
	if len(match) < 2 {
		return ""
	}

	normalized := normalizeCodexCLIVersion(match[1])
	if _, ok := parseCodexSemver(normalized); !ok {
		return ""
	}
	return normalized
}

func normalizeCodexCLIVersion(version string) string {
	main, prerelease, hasPrerelease := strings.Cut(strings.TrimSpace(version), "-")
	segments := make([]string, 0, 3)
	for _, segment := range strings.Split(main, ".") {
		segment = strings.TrimSpace(segment)
		if segment != "" {
			segments = append(segments, segment)
		}
	}

	if len(segments) == 2 {
		segments = append(segments, "0")
	}

	normalized := strings.Join(segments, ".")
	if hasPrerelease {
		return normalized + "-" + prerelease
	}
	return normalized
}

func parseCodexSemver(version string) (parsedCodexSemver, bool) {
	main, prerelease, _ := strings.Cut(version, "-")
	segments := strings.Split(main, ".")
	if len(segments) != 3 {
		return parsedCodexSemver{}, false
	}

	major, err := strconv.Atoi(segments[0])
	if err != nil {
		return parsedCodexSemver{}, false
	}
	minor, err := strconv.Atoi(segments[1])
	if err != nil {
		return parsedCodexSemver{}, false
	}
	patch, err := strconv.Atoi(segments[2])
	if err != nil {
		return parsedCodexSemver{}, false
	}

	parsed := parsedCodexSemver{
		major: major,
		minor: minor,
		patch: patch,
	}
	if prerelease != "" {
		for _, segment := range strings.Split(prerelease, ".") {
			segment = strings.TrimSpace(segment)
			if segment != "" {
				parsed.prerelease = append(parsed.prerelease, segment)
			}
		}
	}

	return parsed, true
}

func compareCodexCLIVersions(left, right string) int {
	parsedLeft, okLeft := parseCodexSemver(normalizeCodexCLIVersion(left))
	parsedRight, okRight := parseCodexSemver(normalizeCodexCLIVersion(right))
	if !okLeft || !okRight {
		return strings.Compare(left, right)
	}

	// The numeric half is the shared ordering (semver.go); only the
	// prerelease tail below is Codex-specific.
	if numeric := CompareSemverTriple(
		[3]int{parsedLeft.major, parsedLeft.minor, parsedLeft.patch},
		[3]int{parsedRight.major, parsedRight.minor, parsedRight.patch},
	); numeric != 0 {
		return numeric
	}

	if len(parsedLeft.prerelease) == 0 && len(parsedRight.prerelease) == 0 {
		return 0
	}
	if len(parsedLeft.prerelease) == 0 {
		return 1
	}
	if len(parsedRight.prerelease) == 0 {
		return -1
	}

	length := len(parsedLeft.prerelease)
	if len(parsedRight.prerelease) > length {
		length = len(parsedRight.prerelease)
	}
	for index := 0; index < length; index++ {
		if index >= len(parsedLeft.prerelease) {
			return -1
		}
		if index >= len(parsedRight.prerelease) {
			return 1
		}
		comparison := compareCodexPrereleaseIdentifier(
			parsedLeft.prerelease[index],
			parsedRight.prerelease[index],
		)
		if comparison != 0 {
			return comparison
		}
	}

	return 0
}

func compareCodexPrereleaseIdentifier(left, right string) int {
	leftNumeric := isNumericString(left)
	rightNumeric := isNumericString(right)

	switch {
	case leftNumeric && rightNumeric:
		leftValue, _ := strconv.Atoi(left)
		rightValue, _ := strconv.Atoi(right)
		return leftValue - rightValue
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	default:
		return strings.Compare(left, right)
	}
}

func isNumericString(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseCodexCLIVersion extracts the first semver-shaped token from arbitrary
// Codex version text — `codex --version` output, or the `userAgent` string the
// app-server returns from `initialize` (`codex_cli_rs/0.149.0 (Linux …)`).
// Returns "" when nothing parses, which callers must treat as "unknown", never
// as "old" or "new".
//
// Exported so the codex adapter reads the version of the process it is
// talking to with the same parser detection uses.
func ParseCodexCLIVersion(output string) string { return parseCodexCLIVersion(output) }
