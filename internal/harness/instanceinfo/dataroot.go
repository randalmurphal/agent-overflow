package instanceinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// The per-worktree default data root: where an isolated boot puts its
// data when nobody named a directory.
//
// The rule lives here rather than in package main because three parties
// must agree on it and none of them can import the others: the backend's
// own flag defaults (`--soak --window`), the Makefile's HARNESS_DATA_DIR
// (`$(HARNESS_TMPDIR)/agent-overflow-harness$(subst /,-,$(CURDIR))`,
// where HARNESS_TMPDIR mirrors os.TempDir()'s $TMPDIR-else-/tmp rule —
// dataroot_test.go pins the Makefile's spelling), and the
// ao-harness CLI, which has to name the instance belonging to the
// checkout it was invoked from without having parsed anyone's stdout.
// A second spelling of this rule means `make harness` and `ao-harness
// list` disagree about which instance a worktree has.

// DataRootPrefix is the shared prefix of every per-worktree data root.
// The checkout path (separators flattened) is appended verbatim,
// leading separator included, exactly as `$(subst /,-,$(CURDIR))` does.
const DataRootPrefix = "agent-overflow-harness"

// SoakSuffix distinguishes a checkout's windowed-soak root from its
// harness root. They cannot be the same directory: the soak autopilot
// refuses a data dir holding threads it did not seed, so `make harness`
// followed by `make soak-window` would fail on the second boot.
const SoakSuffix = "-soak"

// DataRootFor is the pure derivation: one data root per checkout, so two
// worktrees running instances at once never share a database and the
// same worktree restarted reuses its seeded state.
func DataRootFor(checkout string) string {
	// Windows drive letters carry a colon, which cannot appear inside a
	// path component; it flattens with the separators.
	flattened := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(filepath.Clean(checkout))
	return filepath.Join(os.TempDir(), DataRootPrefix+flattened)
}

// SoakDataRootFor is DataRootFor with the soak suffix.
func SoakDataRootFor(checkout string) string {
	return DataRootFor(checkout) + SoakSuffix
}

// DefaultDataRoot is DataRootFor the current working directory. An
// unreadable cwd (a deleted checkout) degrades to the bare prefix rather
// than failing: naming SOME isolated scratch directory is better than
// refusing to boot over a directory nobody can name anyway.
func DefaultDataRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Join(os.TempDir(), DataRootPrefix)
	}
	return DataRootFor(cwd)
}

// DefaultSoakDataRoot is DefaultDataRoot with the soak suffix.
func DefaultSoakDataRoot() string {
	return DefaultDataRoot() + SoakSuffix
}
