package git

import (
	"fmt"
	"strings"
)

// PruneCandidate is a local branch whose configured upstream is gone —
// it was pushed and the remote side has since been deleted (merge +
// delete-branch, typically). Never-pushed local branches are excluded
// by construction: without an upstream there is nothing to be "gone".
type PruneCandidate struct {
	Name string
	// Tip is the branch head's full object name; UIs shorten for display.
	Tip string
	// Subject is the tip commit's subject line, for the preview dialog.
	Subject string
	// MergedIntoDefault is true when the tip is reachable from the
	// default branch — deleting loses nothing. Squash-merged branches
	// read false here (their tip is not an ancestor of the squash
	// commit); callers disambiguate those via forge merged-PR heads.
	MergedIntoDefault bool
}

// ListPruneCandidates returns local branches eligible for pruning:
// upstream gone, not the default branch, and not checked out in any
// worktree (git would refuse to delete a checked-out branch anyway,
// and a checkout is exactly the "worktree still using it" signal).
func (c *Core) ListPruneCandidates(cwd string) ([]PruneCandidate, error) {
	defaultBranch, err := c.defaultBranchName(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve default branch: %w", err)
	}

	// NUL-separated: no field can contain a NUL byte, so branch names or
	// subjects holding any printable delimiter can never shift columns.
	result, err := c.run(cwd, "for-each-ref", "refs/heads",
		"--format=%(refname:short)%00%(worktreepath)%00%(upstream:track,nobracket)%00%(objectname)%00%(contents:subject)")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git for-each-ref failed: %s", strings.TrimSpace(result.stderr))
	}

	type parsedRef struct {
		name         string
		worktreePath string
		gone         bool
		tip          string
		subject      string
	}
	var refs []parsedRef
	for _, line := range strings.Split(result.stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 5)
		if len(parts) != 5 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		refs = append(refs, parsedRef{
			name:         name,
			worktreePath: strings.TrimSpace(parts[1]),
			gone:         strings.TrimSpace(parts[2]) == "gone",
			tip:          strings.TrimSpace(parts[3]),
			subject:      strings.TrimSpace(parts[4]),
		})
	}

	// origin/HEAD can be unset (manual remote add, partial clones); fall
	// back to a local main/master so the merged check still runs. With
	// no resolvable default at all, candidates surface unmerged — the
	// preview keeps them unchecked rather than guessing.
	mergeBase := defaultBranch
	if mergeBase == "" {
		for _, conventional := range []string{"main", "master"} {
			for _, ref := range refs {
				if ref.name == conventional {
					mergeBase = conventional
					break
				}
			}
			if mergeBase != "" {
				break
			}
		}
	}
	merged := map[string]bool{}
	if mergeBase != "" {
		merged, err = c.mergedLocalBranches(cwd, mergeBase)
		if err != nil {
			return nil, err
		}
	}

	var candidates []PruneCandidate
	for _, ref := range refs {
		if !ref.gone || ref.worktreePath != "" {
			continue
		}
		if ref.name == mergeBase || isDefaultBranchName(ref.name, defaultBranch) {
			continue
		}
		candidates = append(candidates, PruneCandidate{
			Name:              ref.name,
			Tip:               ref.tip,
			Subject:           ref.subject,
			MergedIntoDefault: merged[ref.name],
		})
	}
	return candidates, nil
}

// mergedLocalBranches returns the set of local branch names whose tips
// are reachable from mergeBase.
func (c *Core) mergedLocalBranches(cwd, mergeBase string) (map[string]bool, error) {
	result, err := c.run(cwd, "for-each-ref", "refs/heads",
		"--merged="+mergeBase, "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git for-each-ref --merged failed: %s", strings.TrimSpace(result.stderr))
	}
	merged := make(map[string]bool)
	for _, line := range strings.Split(result.stdout, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			merged[name] = true
		}
	}
	return merged, nil
}

// DeleteLocalBranch force-deletes a local branch. Callers are expected
// to have established the branch is safe to drop (prune preview /
// candidate re-validation); -D is required because squash-merged
// branches are "unmerged" in git's -d sense.
func (c *Core) DeleteLocalBranch(cwd, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("branch name is required")
	}
	// "--" so a flag-shaped name can never parse as a git option — this
	// argv carries a wire-supplied branch name.
	result, err := c.run(cwd, "branch", "-D", "--", trimmed)
	if err != nil {
		return err
	}
	if result.exitCode != 0 {
		return fmt.Errorf("git branch -D failed: %s", strings.TrimSpace(result.stderr))
	}
	return nil
}
