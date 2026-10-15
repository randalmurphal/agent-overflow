package git

import (
	"fmt"
	"strings"
)

// PRRevision is the pull request state a local read is computed at, as the
// forge last reported it: the head commit and the base branch's tip. Every
// PR read of one snapshot (diff, commit list, merge conflicts) uses the
// same pair, so they agree with each other and with the forge.
type PRRevision struct {
	BaseRef string `json:"baseRef"`
	// BaseSHA is the base branch's tip (PRDetail.BaseRefSHA).
	BaseSHA string `json:"baseSHA"`
	HeadSHA string `json:"headSHA"`
}

// Validate reports whether r names a base branch and two full object IDs.
func (r PRRevision) Validate() error {
	if strings.TrimSpace(r.BaseRef) == "" {
		return fmt.Errorf("base branch is required")
	}
	if err := ValidateBranchName(r.BaseRef); err != nil {
		return err
	}
	if !treeOIDPattern.MatchString(r.BaseSHA) {
		return fmt.Errorf("base commit %q is not a full object ID", r.BaseSHA)
	}
	if !treeOIDPattern.MatchString(r.HeadSHA) {
		return fmt.Errorf("head commit %q is not a full object ID", r.HeadSHA)
	}
	return nil
}

// EnsurePRObjects makes rev's head and base commits present in cwd's
// repository. Both are usually local already (the branch was pushed from
// here, or a fetch brought them), and then nothing touches the network.
// Otherwise one fetch from origin asks for the PR head ref and the base
// branch, the refs the forge publishes those commits under.
//
// Success is the commits being present afterwards, not the fetch's exit
// status: a fetch that transferred everything but lost a ref update to
// another git process still did its job. A fetch failure is returned only
// when a commit is still missing, and a commit origin does not have (the
// PR moved after the snapshot) is reported as such.
func (c *Core) EnsurePRObjects(cwd string, pr PRReference, rev PRRevision) error {
	if err := rev.Validate(); err != nil {
		return err
	}
	missing, err := c.missingCommits(cwd, rev.HeadSHA, rev.BaseSHA)
	if err != nil || len(missing) == 0 {
		return err
	}
	headRef, err := PRHeadRef(pr.Forge, pr.Number)
	if err != nil {
		return err
	}
	_, _, fetchErr := c.executeInteractive(cwd, "fetch", "origin", headRef, rev.BaseRef)
	missing, err = c.missingCommits(cwd, missing...)
	if err != nil || len(missing) == 0 {
		return err
	}
	if fetchErr != nil {
		return fmt.Errorf("fetch pull request commits: %w", fetchErr)
	}
	return fmt.Errorf("origin has no commit %s for %s or %s; the pull request changed since the forge reported it",
		strings.Join(missing, ", "), headRef, rev.BaseRef)
}

// missingCommits returns the oids that do not name a commit in cwd's
// repository, in one `git cat-file --batch-check`.
func (c *Core) missingCommits(cwd string, oids ...string) ([]string, error) {
	var input strings.Builder
	for _, oid := range oids {
		input.WriteString(oid + "^{commit}\n")
	}
	stdout, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, stdin: input.String(), args: []string{"cat-file", "--batch-check"}})
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != len(oids) {
		return nil, fmt.Errorf("git cat-file --batch-check answered %d lines for %d objects", len(lines), len(oids))
	}
	var missing []string
	for i, line := range lines {
		if strings.HasSuffix(line, " missing") {
			missing = append(missing, oids[i])
		}
	}
	return missing, nil
}
