package forgefake

import (
	"fmt"
	"os/exec"
	"strings"
)

// What `gh pr create` (ghPRCreate) and `glab mr create` (glabMRCreate)
// share. Both open one for the checkout's current branch in the repository its
// origin names, into the repository's default branch unless the call names
// a base. The fixture has no default-branch field; it is "main", the
// default a seeded pull's BaseRef takes.

const defaultBaseRef = "main"

// CheckoutHead is the branch and commit a checkout has checked out.
type CheckoutHead struct {
	Branch string
	SHA    string
}

// gitHead reads the checkout's current branch and commit the way the CLIs
// do. A detached HEAD has no branch and is an error.
func gitHead(cwd string) (CheckoutHead, error) {
	branch, err := exec.Command("git", "-C", cwd, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return CheckoutHead{}, fmt.Errorf("could not determine the current branch of %s: %w", cwd, err)
	}
	sha, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	if err != nil {
		return CheckoutHead{}, fmt.Errorf("could not read HEAD of %s: %w", cwd, err)
	}
	return CheckoutHead{Branch: strings.TrimSpace(string(branch)), SHA: strings.TrimSpace(string(sha))}, nil
}

// createRequest is one create call after its CLI's flags are read.
type createRequest struct {
	title string
	body  string
	base  string
	draft bool
}

// createTarget resolves the repository and head a create call acts on.
// Callers hold mu.
func (e *Engine) createTarget(forge string, c *call) (*Repo, CheckoutHead, error) {
	r, err := e.repoForCheckout(forge, c.cwd)
	if err != nil {
		return nil, CheckoutHead{}, err
	}
	head, err := e.opts.Head(c.cwd)
	if err != nil {
		return nil, CheckoutHead{}, err
	}
	return r, head, nil
}

// openPullFor returns the open pull whose head is branch and base is base.
func (r *Repo) openPullFor(branch, base string) *Pull {
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if p.State == "open" && p.HeadRef == branch && p.BaseRef == base {
			return p
		}
	}
	return nil
}

// addPull records a new open pull for head and returns it. Callers hold mu
// and have refused a duplicate.
func (e *Engine) addPull(r *Repo, head CheckoutHead, req createRequest) (*Pull, error) {
	number := 1
	for _, p := range r.Pulls {
		if p.Number >= number {
			number = p.Number + 1
		}
	}
	pull := Pull{
		Number:  number,
		Title:   req.title,
		Body:    req.body,
		Draft:   req.draft,
		Author:  e.viewer,
		HeadRef: head.Branch,
		BaseRef: req.base,
		HeadSHA: head.SHA,
	}
	if err := pull.normalize(r.Forge, &e.ids); err != nil {
		return nil, err
	}
	r.Pulls = append(r.Pulls, pull)
	return &r.Pulls[len(r.Pulls)-1], nil
}
