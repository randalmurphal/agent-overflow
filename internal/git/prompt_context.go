package git

import (
	"fmt"
	"strconv"
	"strings"
)

// PromptSnapshotLimits bound the sections of a PromptSnapshot. The
// snapshot is embedded in a system prompt, where every byte is context the
// model pays for on every turn, so it is deliberately far tighter than the
// commit-context limits.
const (
	// PromptStatusLimit caps the short-status section. A working tree with
	// hundreds of dirty files says "dirty" long before the list ends.
	PromptStatusLimit = 4_000
	// PromptRecentCommits is how many one-line commits the snapshot carries.
	PromptRecentCommits = 5
)

// PromptSnapshot is the compact repository picture a system-prompt
// override renders into its git placeholders. It is a snapshot for the
// model to read, never data the app acts on — contrast GitStatus, which
// drives UI state and gating.
type PromptSnapshot struct {
	// IsRepo is false for a workspace that is not inside a git
	// repository. That is a normal answer, not an error, and the other
	// fields are then empty.
	IsRepo bool
	// Branch is the checked-out branch, empty on a detached HEAD.
	Branch string
	// ShortStatus is `git status --short` output, truncated at
	// PromptStatusLimit. Empty on a clean tree.
	ShortStatus string
	// RecentCommits holds up to PromptRecentCommits `<short-sha> <subject>`
	// lines, newest first. Empty on a repository with no commits yet.
	RecentCommits []string
}

// PromptBlock formats the snapshot the way Claude's own gitStatus section
// reads, so a prompt migrated off the default keeps its shape. Sections
// with nothing to say are omitted entirely rather than left as an empty
// heading — a detached HEAD has no branch line, a clean tree no status.
//
// The result never ends in a newline: it is substituted into the middle of
// a user-authored prompt, where the surrounding text owns the spacing.
func (s PromptSnapshot) PromptBlock() string {
	var b strings.Builder
	if s.Branch != "" {
		fmt.Fprintf(&b, "Current branch: %s\n", s.Branch)
	}
	if s.ShortStatus != "" {
		b.WriteString("Status:\n")
		b.WriteString(s.ShortStatus)
		b.WriteString("\n")
	}
	if len(s.RecentCommits) > 0 {
		b.WriteString("Recent commits:\n")
		b.WriteString(strings.Join(s.RecentCommits, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// PromptSnapshot reads the compact repository picture for cwd.
//
// Two git invocations, no network and no forge lookup: this runs on the
// session spawn path, where StatusFast's six probes (and Status's PR
// lookup) would be paid for facts the prompt never shows. A cwd outside a
// repository returns IsRepo=false with a nil error; an error means git
// itself failed and the caller decides whether to degrade.
func (c *Core) PromptSnapshot(cwd string) (PromptSnapshot, error) {
	// runLocaleC: the non-repo classification below matches git's own
	// English message, which a git built with NLS would otherwise
	// translate — misreporting a plain non-repo path as a hard error.
	result, err := c.runLocaleC(cwd, "status", "--short", "--branch")
	if err != nil {
		return PromptSnapshot{}, err
	}
	if result.exitCode != 0 {
		if strings.Contains(strings.ToLower(result.stderr), "not a git repository") {
			return PromptSnapshot{IsRepo: false}, nil
		}
		return PromptSnapshot{}, fmt.Errorf("git status --short failed: %s", strings.TrimSpace(result.stderr))
	}

	branch, status := splitShortStatus(result.stdout)
	return PromptSnapshot{
		IsRepo:        true,
		Branch:        branch,
		ShortStatus:   limitSection(status, PromptStatusLimit),
		RecentCommits: c.recentCommitLines(cwd, PromptRecentCommits),
	}, nil
}

// recentCommitLines returns `<short-sha> <subject>` for the newest n
// commits, newest first. Best-effort like RecentCommitSubjects: an
// unborn branch has no commits and that is not a failure.
func (c *Core) recentCommitLines(cwd string, n int) []string {
	if n <= 0 {
		return nil
	}
	result, err := c.run(cwd, "log", "-n", strconv.Itoa(n), "--pretty=format:%h %s")
	if err != nil || result.exitCode != 0 {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(result.stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// splitShortStatus separates `git status --short --branch` output into the
// branch name and the remaining entry lines.
//
// The `## ` header has four shapes worth handling: `## main`,
// `## main...origin/main [ahead 1]`, `## No commits yet on main` (unborn
// branch) and `## HEAD (no branch)` (detached, which yields an empty
// branch rather than the literal "HEAD").
//
// Accumulation stops once the status has passed PromptStatusLimit: the
// caller truncates to exactly that many bytes, so a working tree with tens
// of thousands of dirty paths would otherwise build a multi-megabyte
// intermediate for a 4KB answer. Stopping only AFTER the limit is exceeded
// is what keeps the truncated output byte-identical to the unbounded one.
// Scanning continues past that point when the header has not been seen yet
// — git puts it first, but losing the branch name to a line-order
// assumption would be a silent wrong answer rather than a loud one.
func splitShortStatus(stdout string) (branch, status string) {
	var b strings.Builder
	branchSeen := false
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.HasPrefix(line, "## ") {
			branch = parseShortStatusBranch(strings.TrimPrefix(line, "## "))
			branchSeen = true
			if b.Len() > PromptStatusLimit {
				break
			}
			continue
		}
		if b.Len() > PromptStatusLimit {
			if branchSeen {
				break
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return branch, b.String()
}

func parseShortStatusBranch(header string) string {
	header = strings.TrimSpace(header)
	if header == "HEAD (no branch)" {
		return ""
	}
	header = strings.TrimPrefix(header, "No commits yet on ")
	if idx := strings.Index(header, "..."); idx >= 0 {
		header = header[:idx]
	}
	if idx := strings.Index(header, " ["); idx >= 0 {
		header = header[:idx]
	}
	return strings.TrimSpace(header)
}
