package provider

// semver.go — the one dotted-triple comparison both provider CLIs' version
// gates order on.
//
// Only the COMPARE is shared. Each CLI prints its version differently and each
// parser keeps its own tolerance for that: Codex's accepts a `v` prefix, a
// two-segment version, and a prerelease suffix (`ParseCodexCLIVersion`), while
// Claude's accepts a trailing token (`claude --version` prints
// "2.1.237 (Claude Code)" where `system/init` carries the bare number). What
// they must NOT disagree about is which of two triples is newer — a copy of
// that arithmetic per provider is a silent ordering bug waiting for its first
// two-digit component.

// CompareSemverTriple orders two `major.minor.patch` triples: negative when
// left is older, zero when equal, positive when left is newer.
func CompareSemverTriple(left, right [3]int) int {
	for i := range left {
		if left[i] != right[i] {
			return left[i] - right[i]
		}
	}
	return 0
}

// SemverAtLeast reports whether the have triple is at or above the want
// triple. Callers parse with their own CLI's tolerance and pass the result
// here; an unparseable version is the caller's fail-closed decision, not this
// function's — it has no way to express "unknown".
func SemverAtLeast(have, want [3]int) bool {
	return CompareSemverTriple(have, want) >= 0
}
