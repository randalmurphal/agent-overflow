package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PRLocalBranch is the local branch a pull request whose head lives in
// another repository (a fork) is checked out on: "pr/N". Its head branch
// is not origin's, so the forge's head ref is the only way to it.
func PRLocalBranch(number int) string {
	return "pr/" + strconv.Itoa(number)
}

// ErrPRBranchDiverged is a local pr/N branch that already exists at
// another commit than the pull request's head: it may hold work, so it
// is never moved.
var ErrPRBranchDiverged = errors.New("the local branch is not at the pull request's head")

// FetchPRBranch fetches pr's head from origin's forge head ref (PRHeadRef)
// into the local branch PRLocalBranch(pr.Number) and returns that branch
// with the head commit. A branch that does not exist is created at the
// head; one already at the head is used as it is; one at another commit
// fails with ErrPRBranchDiverged naming both commits. The fetch lands in
// a ref of its own, deleted afterwards, so a concurrent fetch writing
// FETCH_HEAD cannot change the answer. The fetch is the user's action
// (a credential prompt may answer it) and ends with ctx.
func (c *Core) FetchPRBranch(ctx context.Context, cwd string, pr PRReference) (branch, head string, err error) {
	if err := pr.Validate(); err != nil {
		return "", "", err
	}
	headRef, err := PRHeadRef(pr.Forge, pr.Number)
	if err != nil {
		return "", "", err
	}
	branch = PRLocalBranch(pr.Number)
	if err := validateBranchName(branch); err != nil {
		return "", "", err
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", headRef, err)
	}
	scratch := "refs/agent-overflow/pr-fetch/" + hex.EncodeToString(nonce)
	git := func(interactive bool, args ...string) (commandResult, error) {
		return c.runSpec(commandSpec{ctx: ctx, binary: "git", cwd: cwd, allowCredentialPrompt: interactive, args: args})
	}
	result, err := git(true, "fetch", "--no-tags", "origin", "+refs/"+headRef+":"+scratch)
	if err == nil && result.exitCode != 0 {
		err = exitError(commandSpec{binary: "git", args: []string{"fetch", "origin", headRef}}, result)
	}
	if err != nil {
		return "", "", fmt.Errorf("fetch %s from origin: %w", headRef, err)
	}
	defer func() {
		// The scratch ref is ours alone; a failure to drop it leaves a ref
		// no reader lists, so it is reported beside the answer, not instead
		// of it.
		if drop, dropErr := git(false, "update-ref", "-d", scratch); dropErr != nil || drop.exitCode != 0 {
			err = errors.Join(err, fmt.Errorf("remove %s: %v %s", scratch, dropErr, strings.TrimSpace(drop.stderr)))
		}
	}()
	resolved, err := git(false, "rev-parse", "--verify", "--quiet", scratch+"^{commit}")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(resolved.stdout)
	if resolved.exitCode != 0 || !treeOIDPattern.MatchString(head) {
		return "", "", fmt.Errorf("fetch %s from origin: no commit arrived", headRef)
	}
	// An empty old value creates the branch only where none exists, in one
	// ref transaction.
	created, err := git(false, "update-ref", "refs/heads/"+branch, head, "")
	if err != nil {
		return "", "", err
	}
	if created.exitCode == 0 {
		return branch, head, nil
	}
	existing, err := git(false, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", "", err
	}
	if existing.exitCode != 0 {
		return "", "", fmt.Errorf("create branch %s: %s", branch, strings.TrimSpace(created.stderr))
	}
	if at := strings.TrimSpace(existing.stdout); at != head {
		return "", "", fmt.Errorf("branch %s is at %s, and %s's head is %s: %w", branch, shortOID(at), headRef, shortOID(head), ErrPRBranchDiverged)
	}
	return branch, head, nil
}

func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
