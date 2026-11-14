package git

import (
	"fmt"
	"strings"
)

// PRRevision is the pull request state a local read is computed at, as the
// forge last reported it: the head commit and the base commit the forge
// recorded for the PR. The diff and the commit list of one snapshot use the
// same pair, so they agree with each other and with the forge. The merge
// conflict read merges the head with the base branch's live tip instead
// (RemoteBranchTip), because the recorded base does not follow the branch.
type PRRevision struct {
	BaseRef string `json:"baseRef"`
	// BaseSHA is the base commit the forge recorded for the PR
	// (PRDetail.BaseRefSHA). It is not the base branch's current tip:
	// GitLab's start_sha is the target tip when the diff was last
	// computed, usually the merge base, and GitHub's baseRefOid can lag
	// the branch by several merges.
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
//
// EnsurePRObjects is for a read the user asked for, so the fetch may ask
// for credentials; EnsurePRObjectsUnattended is the same read run
// automatically.
func (c *Core) EnsurePRObjects(cwd string, pr PRReference, rev PRRevision) error {
	return c.ensurePRObjects(cwd, pr, rev, true)
}

// EnsurePRObjectsUnattended is EnsurePRObjects for a read nobody asked for
// at that moment (a reconcile on a forge poll, a retry after a failed
// read): a credential prompt fails fast instead of opening a dialog.
func (c *Core) EnsurePRObjectsUnattended(cwd string, pr PRReference, rev PRRevision) error {
	return c.ensurePRObjects(cwd, pr, rev, false)
}

func (c *Core) ensurePRObjects(cwd string, pr PRReference, rev PRRevision, allowCredentialPrompt bool) error {
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
	_, _, fetchErr := c.executeNetwork(cwd, allowCredentialPrompt, "fetch", "origin", headRef, rev.BaseRef)
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

// RemoteBranchTip returns the commit branch names on origin now, with its
// objects present in cwd's repository. `git ls-remote` resolves the tip;
// when that commit is already local nothing else touches the network and no
// ref moves. Otherwise one fetch of the branch downloads it, and, as with
// EnsurePRObjects, the fetch names the branch, so git also moves
// origin/<branch>. Success is the commit being present afterwards, not the
// fetch's exit status. A tip still missing after a successful fetch means
// the branch moved between the two calls; that is an error, not a retry.
//
// RemoteBranchTip is for a read the user asked for, so both network steps
// may ask for credentials; RemoteBranchTipUnattended is the same read run
// automatically.
func (c *Core) RemoteBranchTip(cwd, branch string) (string, error) {
	return c.remoteBranchTip(cwd, branch, true)
}

// RemoteBranchTipUnattended is RemoteBranchTip for an automatic read: a
// credential prompt fails fast instead of opening a dialog.
func (c *Core) RemoteBranchTipUnattended(cwd, branch string) (string, error) {
	return c.remoteBranchTip(cwd, branch, false)
}

func (c *Core) remoteBranchTip(cwd, branch string, allowCredentialPrompt bool) (string, error) {
	if err := ValidateBranchName(branch); err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	stdout, _, err := c.executeNetwork(cwd, allowCredentialPrompt, "ls-remote", "origin", ref)
	if err != nil {
		return "", fmt.Errorf("read %s on origin: %w", branch, err)
	}
	tip, err := lsRemoteTip(stdout, ref)
	if err != nil {
		return "", fmt.Errorf("read %s on origin: %w", branch, err)
	}
	missing, err := c.missingCommits(cwd, tip)
	if err != nil || len(missing) == 0 {
		return tip, err
	}
	_, _, fetchErr := c.executeNetwork(cwd, allowCredentialPrompt, "fetch", "origin", ref)
	missing, err = c.missingCommits(cwd, tip)
	if err != nil {
		return "", err
	}
	if len(missing) == 0 {
		return tip, nil
	}
	if fetchErr != nil {
		return "", fmt.Errorf("fetch %s from origin: %w", branch, fetchErr)
	}
	return "", fmt.Errorf("%s moved on origin during the read: it named %s, which the fetch did not bring; try again", branch, tip)
}

// lsRemoteTip returns the object ID of the one `git ls-remote` line
// ("<oid>\t<ref>") naming ref exactly. ls-remote matches its pattern
// against trailing path components, so lines for other refs are skipped.
func lsRemoteTip(stdout, ref string) (string, error) {
	tip := ""
	for _, line := range strings.Split(stdout, "\n") {
		oid, name, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "\t")
		if !ok || name != ref {
			continue
		}
		if tip != "" {
			return "", fmt.Errorf("git ls-remote listed %s more than once", ref)
		}
		if !treeOIDPattern.MatchString(oid) {
			return "", fmt.Errorf("git ls-remote listed %s at %q, not an object ID", ref, oid)
		}
		tip = oid
	}
	if tip == "" {
		return "", fmt.Errorf("origin has no branch %s", strings.TrimPrefix(ref, "refs/heads/"))
	}
	return tip, nil
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
