package provider

// semver.go — the dotted-triple comparison Claude's version gate orders on.
// Claude's parser accepts a trailing token (`claude --version` prints
// "2.1.237 (Claude Code)" where `system/init` carries the bare number) and
// passes the triple here. Codex builds are strict SemVer with prereleases,
// and codex_version.go orders them by SemVer precedence instead
// (CompareCodexVersions).

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
