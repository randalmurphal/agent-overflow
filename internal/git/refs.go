package git

import (
	"fmt"
	"strconv"
	"strings"
)

// TagRefPrefix marks a tag on the wire: a tag travels as
// "refs/tags/<name>". A branch travels as its short name, or as
// "refs/heads/<name>" (BranchRefPrefix) when spelled explicitly, which
// Ref.Wire does for a short name that itself begins with "refs/", so every
// branch and tag name round-trips. ParseRef is the only reader of the
// prefixes and Ref.Wire the only writer.
const TagRefPrefix = "refs/tags/"

// BranchRefPrefix marks a branch spelled explicitly on the wire.
const BranchRefPrefix = "refs/heads/"

// RefKind says what a Ref names.
type RefKind uint8

const (
	// RefHead is the zero kind: no ref named, the repository's HEAD.
	RefHead RefKind = iota
	// RefBranch is a branch by its short name.
	RefBranch
	// RefTag is a tag by its name without the refs/tags/ prefix.
	RefTag
	// RefCommit is a full commit id, the current ref of a detached HEAD at
	// no tag. It never comes from the wire.
	RefCommit
)

// Ref is a typed branch, tag or commit. Every git argument built from a Ref
// is unambiguous: a name that is both a branch and a tag resolves to the
// kind the Ref carries, never to git's guess.
type Ref struct {
	Kind RefKind
	Name string
}

// BranchRef is the Ref of branch name.
func BranchRef(name string) Ref { return Ref{Kind: RefBranch, Name: name} }

// TagRef is the Ref of tag name.
func TagRef(name string) Ref { return Ref{Kind: RefTag, Name: name} }

// CommitRef is the Ref of a commit id.
func CommitRef(sha string) Ref { return Ref{Kind: RefCommit, Name: sha} }

// ParseRef reads a wire ref: "refs/heads/<name>" is branch <name> (exactly
// one prefix is stripped), "refs/tags/<name>" is a tag, any other
// non-empty string a branch by its short name. An empty string is the
// zero Ref (HEAD). The name is validated so it cannot reach argv as a flag.
func ParseRef(wire string) (Ref, error) {
	ref, _, err := parseRef(wire)
	return ref, err
}

// parseRef is ParseRef that also reports whether wire was spelled with
// refs/heads/ or refs/tags/, naming its kind explicitly. explicit is
// reported even when the name fails validation.
func parseRef(wire string) (ref Ref, explicit bool, err error) {
	wire = strings.TrimSpace(wire)
	if wire == "" {
		return Ref{}, false, nil
	}
	ref = BranchRef(wire)
	if name, ok := strings.CutPrefix(wire, BranchRefPrefix); ok {
		ref, explicit = BranchRef(name), true
	} else if name, ok := strings.CutPrefix(wire, TagRefPrefix); ok {
		ref, explicit = TagRef(name), true
	}
	if err := ref.validate(); err != nil {
		return Ref{}, explicit, err
	}
	return ref, explicit, nil
}

// IsQualifiedRef reports whether wire is spelled "refs/heads/<name>" or
// "refs/tags/<name>", so a caller that tidies a plain branch name it was
// given never rewrites one of these into a different ref.
func IsQualifiedRef(wire string) bool {
	_, explicit, _ := parseRef(wire)
	return explicit
}

// IsZero reports whether r names nothing, meaning HEAD.
func (r Ref) IsZero() bool { return r.Kind == RefHead }

// IsTag reports whether r is a tag.
func (r Ref) IsTag() bool { return r.Kind == RefTag }

// IsBranch reports whether r is a branch.
func (r Ref) IsBranch() bool { return r.Kind == RefBranch }

// Wire is r as the wire carries it: a branch's short name, or
// "refs/heads/<name>" when the short name begins with "refs/"; a tag as
// "refs/tags/<name>"; a commit's id; "" for HEAD. ParseRef reads it back
// as r.
func (r Ref) Wire() string {
	switch {
	case r.Kind == RefTag:
		return TagRefPrefix + r.Name
	case r.Kind == RefBranch && strings.HasPrefix(r.Name, "refs/"):
		return BranchRefPrefix + r.Name
	}
	return r.Name
}

// Label is r for a person: the branch or tag name, a commit's short id.
func (r Ref) Label() string {
	if r.Kind == RefCommit && len(r.Name) > 12 {
		return r.Name[:12]
	}
	return r.Name
}

// revision is r as an unambiguous git revision: fully qualified for a
// branch or tag, the id for a commit, "" for HEAD.
func (r Ref) revision() string {
	switch r.Kind {
	case RefBranch:
		return BranchRefPrefix + r.Name
	case RefTag:
		return TagRefPrefix + r.Name
	default:
		return r.Name
	}
}

func (r Ref) validate() error {
	switch r.Kind {
	case RefHead:
		return nil
	case RefBranch:
		if r.Name == "" {
			return fmt.Errorf("git branch name is required")
		}
		return validateBranchName(r.Name)
	case RefTag:
		if r.Name == "" {
			return fmt.Errorf("git tag name is required")
		}
		if err := validateBranchName(r.Name); err != nil {
			return fmt.Errorf("invalid tag name %q: %w", r.Name, err)
		}
		return nil
	case RefCommit:
		if !isFullCommitID(r.Name) {
			return fmt.Errorf("invalid commit id %q", r.Name)
		}
		return nil
	default:
		return fmt.Errorf("unknown ref kind %d", r.Kind)
	}
}

func isFullCommitID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// TagExists reports whether tag name exists in cwd's repository.
func (c *Core) TagExists(cwd, name string) (bool, error) {
	ref := TagRef(strings.TrimSpace(name))
	if err := ref.validate(); err != nil {
		return false, err
	}
	return c.refExists(cwd, ref.revision())
}

// refExists reports whether the fully qualified ref exists.
func (c *Core) refExists(cwd, qualified string) (bool, error) {
	result, err := c.run(cwd, "show-ref", "--verify", "--quiet", qualified)
	if err != nil {
		return false, err
	}
	switch result.exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		message := strings.TrimSpace(result.stderr)
		if message == "" {
			message = fmt.Sprintf("git show-ref %s exited with code %d", qualified, result.exitCode)
		}
		return false, fmt.Errorf("%s", message)
	}
}

// ResolveRef resolves a name a person or agent typed to a Ref: a
// "refs/tags/<name>" spelling is that tag and a "refs/heads/<name>" one that
// branch; a bare name is a branch when one exists locally or, unless
// localOnly, as origin's tracking ref from the last fetch, and otherwise a
// tag of that name. found is false when nothing matches. Nothing is
// fetched.
func (c *Core) ResolveRef(cwd, name string, localOnly bool) (ref Ref, found bool, err error) {
	ref, explicit, err := parseRef(name)
	if err != nil || ref.IsZero() {
		return Ref{}, false, err
	}
	if ref.IsBranch() {
		known, err := c.BaseBranchKnown(cwd, ref.Name, localOnly)
		if err != nil || known {
			return ref, known, err
		}
		if explicit {
			return Ref{}, false, nil
		}
		ref = TagRef(ref.Name)
	}
	exists, err := c.TagExists(cwd, ref.Name)
	if err != nil || !exists {
		return Ref{}, false, err
	}
	return ref, true, nil
}

// CurrentRef is cwd's checkout as a Ref: its branch (an unborn one
// included), else the first tag at a detached HEAD (TagAt), else the
// detached HEAD's commit.
func (c *Core) CurrentRef(cwd string) (Ref, error) {
	result, err := c.run(cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return Ref{}, err
	}
	if result.exitCode == 0 {
		if branch := trimBranchRef(strings.TrimSpace(result.stdout)); branch != "" {
			return BranchRef(branch), nil
		}
	}
	if result.exitCode > 1 {
		return Ref{}, fmt.Errorf("git symbolic-ref HEAD failed: %s", strings.TrimSpace(result.stderr))
	}
	head, err := c.headCommit(cwd)
	if err != nil {
		return Ref{}, err
	}
	tag, err := c.TagAt(cwd, head)
	if err != nil {
		return Ref{}, err
	}
	if tag != "" {
		return TagRef(tag), nil
	}
	return CommitRef(head), nil
}

// headCommit is the full id of cwd's HEAD commit.
func (c *Core) headCommit(cwd string) (string, error) {
	stdout, _, err := c.Execute(cwd, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("read HEAD commit: %w", err)
	}
	return strings.TrimSpace(stdout), nil
}

// TagAt is the first tag pointing at commit (a revision such as "HEAD" or
// an id) in ListTags order, creatordate descending then name; "" when no
// tag points at it.
func (c *Core) TagAt(cwd, commit string) (string, error) {
	commit = strings.TrimSpace(commit)
	if commit == "" || strings.HasPrefix(commit, "-") {
		return "", fmt.Errorf("invalid commit %q", commit)
	}
	stdout, _, err := c.Execute(cwd, "for-each-ref", "--count=1", "--points-at="+commit,
		"--sort=refname", "--sort=-creatordate", "--format=%(refname:lstrip=2)", "refs/tags")
	if err != nil {
		return "", fmt.Errorf("read tag at %s: %w", commit, err)
	}
	return strings.TrimSpace(stdout), nil
}

// OrphanCommits counts the commits reachable from commit and from no
// branch, tag or remote-tracking ref: what a detached HEAD loses when it
// moves away or its worktree is removed.
func (c *Core) OrphanCommits(cwd, commit string) (int, error) {
	commit = strings.TrimSpace(commit)
	if commit == "" || strings.HasPrefix(commit, "-") {
		return 0, fmt.Errorf("invalid commit %q", commit)
	}
	stdout, _, err := c.Execute(cwd, "rev-list", "--count", commit, "--not", "--branches", "--tags", "--remotes", "--")
	if err != nil {
		return 0, fmt.Errorf("count orphan commits: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(stdout), err)
	}
	return count, nil
}

// LeftBehind is what moving a detached HEAD away from commit orphans.
type LeftBehind struct {
	Count  int    `json:"count"`
	Commit string `json:"commit"`
}

// DetachedOrphans reports the commits a checkout would leave behind: for a
// detached HEAD, its orphan commits and the HEAD's full id; nil when HEAD is
// on a branch or orphans nothing.
func (c *Core) DetachedOrphans(cwd string) (*LeftBehind, error) {
	head, err := c.detachedCommit(cwd)
	if err != nil || head == "" {
		return nil, err
	}
	count, err := c.OrphanCommits(cwd, head)
	if err != nil || count == 0 {
		return nil, err
	}
	return &LeftBehind{Count: count, Commit: head}, nil
}

// DetachedHead describes cwd's HEAD when it is detached: its full commit id
// and the tag at it (TagAt, "" when none). Both are "" on a branch.
func (c *Core) DetachedHead(cwd string) (commit, tag string, err error) {
	commit, err = c.detachedCommit(cwd)
	if err != nil || commit == "" {
		return "", "", err
	}
	tag, err = c.TagAt(cwd, commit)
	if err != nil {
		return "", "", err
	}
	return commit, tag, nil
}

// detachedCommit is the full id of cwd's HEAD commit when HEAD is detached,
// "" when it is on a branch.
func (c *Core) detachedCommit(cwd string) (string, error) {
	result, err := c.run(cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	switch result.exitCode {
	case 0:
		return "", nil
	case 1:
		return c.headCommit(cwd)
	default:
		return "", fmt.Errorf("git symbolic-ref HEAD failed: %s", strings.TrimSpace(result.stderr))
	}
}
