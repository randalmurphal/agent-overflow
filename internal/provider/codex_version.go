package provider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const minimumCodexCLIVersion = "0.37.0"

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

	if parsedLeft.major != parsedRight.major {
		return parsedLeft.major - parsedRight.major
	}
	if parsedLeft.minor != parsedRight.minor {
		return parsedLeft.minor - parsedRight.minor
	}
	if parsedLeft.patch != parsedRight.patch {
		return parsedLeft.patch - parsedRight.patch
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

func isCodexCLIVersionSupported(version string) bool {
	return compareCodexCLIVersions(version, minimumCodexCLIVersion) >= 0
}

func formatCodexCLIUpgradeMessage(version string) string {
	if version == "" {
		return fmt.Sprintf(
			"Codex CLI is too old for Agent Overflow. Upgrade to v%s or newer and restart the app.",
			minimumCodexCLIVersion,
		)
	}
	return fmt.Sprintf(
		"Codex CLI v%s is too old for Agent Overflow. Upgrade to v%s or newer and restart the app.",
		version,
		minimumCodexCLIVersion,
	)
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
