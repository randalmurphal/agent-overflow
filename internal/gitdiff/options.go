package gitdiff

// Options shapes how a patch is computed. The zero value is the exact,
// canonical patch — every field is an opt-in view change, never a
// correctness change.
type Options struct {
	// IgnoreWhitespace passes `-w` (--ignore-all-space) to the underlying
	// diff, so lines that differ only in whitespace render as unchanged
	// and hunks whose every change is whitespace disappear. This is the
	// review pane's "hide whitespace changes" toggle: an agent that
	// re-indents a block (wrapping it in an `if`, say) otherwise drowns
	// the real edits.
	//
	// Line numbering stays canonical. `-w` narrows and drops hunks but
	// the `@@` ranges it emits are still true file line numbers on both
	// sides, so a (path, line) anchor taken from a `-w` patch names the
	// same physical line it would in the full patch — which is what lets
	// the diff-review comment flow keep working under the toggle. See
	// TestIgnoreWhitespaceKeepsCanonicalLineNumbers.
	//
	// Deliberately NOT --ignore-blank-lines: dropping added/removed blank
	// lines changes which lines exist, not just how they're compared.
	IgnoreWhitespace bool
}

// patchFlags returns the flags shared by every patch-producing invocation
// in this package: a plain unified patch with no color, no repo-defined
// external diff driver, and no textconv filter — the same reasons gitEnv
// scrubs GIT_EXTERNAL_DIFF/GIT_DIFF_OPTS. Options-driven flags append
// after them.
func (o Options) patchFlags() []string {
	flags := make([]string, 0, 6)
	flags = append(flags, "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv")
	if o.IgnoreWhitespace {
		flags = append(flags, "-w")
	}
	return flags
}

// gitArgs builds a full argv: the subcommand, this Options' patch flags,
// then the caller's revision/path arguments.
func (o Options) gitArgs(subcommand string, rest ...string) []string {
	args := make([]string, 0, 2+len(rest)+6)
	args = append(args, subcommand)
	args = append(args, o.patchFlags()...)
	return append(args, rest...)
}
