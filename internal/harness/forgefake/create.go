package forgefake

import (
	"fmt"
	"os/exec"
	"strings"
)

// What `gh pr create` (ghPRCreate) and `glab mr create` (glabMRCreate)
// share. Both open one for the checkout's current branch in the repository
// -R names, else the one its origin names, into the repository's default
// branch unless the call names a base. The fixture has no default-branch field; it is "main", the
// default a seeded pull's BaseRef takes and the default_branch the
// repository identity reports.

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

// createTarget resolves the repository and head a create call acts on:
// the repository -R names (gh: [HOST/]OWNER/REPO; glab: a URL), else the
// one origin names, and the checked-out branch and commit. gh's --head and
// glab's --source-branch may only name the checked-out branch, and glab's
// --head only the -R repository: another head needs a commit or a fork
// the fake does not read. A non-empty refusal is the reason the call is
// unhandled; err is the CLI's own failure. Callers hold mu.
func (e *Engine) createTarget(forge string, c *call) (r *Repo, head CheckoutHead, refusal string, err error) {
	if c.has("repo") {
		if r, refusal, err = e.repoFlag(forge, c.flag("repo")); refusal != "" || err != nil {
			return nil, CheckoutHead{}, refusal, err
		}
	} else if r, err = e.repoForCheckout(forge, c.cwd); err != nil {
		return nil, CheckoutHead{}, "", err
	}
	if head, err = e.opts.Head(c.cwd); err != nil {
		return nil, CheckoutHead{}, "", err
	}
	branchFlag := "head"
	if forge == "gitlab" {
		branchFlag = "source-branch"
		if c.has("head") {
			headRepo, refusal, err := e.repoFlag(forge, c.flag("head"))
			if refusal != "" || err != nil {
				return nil, CheckoutHead{}, refusal, err
			}
			if headRepo != r {
				return nil, CheckoutHead{}, "a head repository other than the target is not implemented", nil
			}
		}
	}
	if branch := c.flag(branchFlag); c.has(branchFlag) && branch != head.Branch {
		return nil, CheckoutHead{}, fmt.Sprintf("--%s %q is not the checked-out branch %q", branchFlag, branch, head.Branch), nil
	}
	return r, head, "", nil
}

// repoFlag resolves a create's -R (or glab's --head) to a seeded
// repository. glab's OWNER/REPO forms leave the host to its configuration,
// which the fake does not read, so only a URL is implemented there.
// Callers hold mu.
func (e *Engine) repoFlag(forge, value string) (*Repo, string, error) {
	var host, project string
	if forge == "github" {
		parts := strings.Split(value, "/")
		switch len(parts) {
		case 2:
			host, project = defaultForgeHost["gh"], value
		case 3:
			host, project = parts[0], parts[1]+"/"+parts[2]
		default:
			return nil, fmt.Sprintf("-R %q is not [HOST/]OWNER/REPO", value), nil
		}
	} else {
		var ok bool
		if host, project, ok = parseRemote(value); !ok {
			return nil, fmt.Sprintf("-R %q is not a repository URL; a repository path without a host is not implemented", value), nil
		}
	}
	r := e.repo(forge, project)
	if r == nil || !strings.EqualFold(r.Host, host) {
		return nil, "", fmt.Errorf("no seeded %s repository %s on %s", forge, project, host)
	}
	return r, "", nil
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
