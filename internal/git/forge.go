package git

import (
	"errors"
	"fmt"
	"strings"
)

// Forge wraps the host-specific operations against a code-hosting CLI
// (gh for GitHub, glab for GitLab). All operations route through the
// owning Core's runBinary so timeouts, size caps, and subprocess
// discipline stay consistent across forges — the Forge implementation
// must not call exec.Command directly.
type Forge interface {
	// ID returns "github" or "gitlab" — the canonical short id for this forge.
	ID() string
	// BinaryName returns the OS binary the forge shells out to (e.g. "gh").
	// Used for "<binary> is not installed" messaging.
	BinaryName() string

	// ListOpenPRs returns open PRs/MRs for the given head/source branch.
	ListOpenPRs(cwd, head string) ([]GitPR, error)
	// CreatePR opens a PR/MR for the current branch in cwd. Returns the URL.
	CreatePR(cwd, title, body string, draft bool) (string, error)
	// ViewPR fetches metadata for a PR/MR identified by project + number.
	// project is "owner/repo" (GitHub) or "namespace/.../repo" (GitLab).
	// cwd may be empty when there is no local clone — gh --repo and
	// glab -R both query authenticated state without needing one.
	ViewPR(cwd, project string, number int) (PRMetadata, error)
	// Diff returns the unified-patch diff for the given PR/MR.
	Diff(cwd, project string, number int) (string, error)
}

// PRMetadata is the forge-agnostic view of a PR/MR fetched via ViewPR.
type PRMetadata struct {
	Title       string
	Body        string
	HeadRefName string
	BaseRefName string
	URL         string
	AuthorLogin string
	State       string
	Files       []PRFile
}

// PRFile describes one file's per-PR change stats.
type PRFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// PRReference identifies a PR/MR by host, namespace, repo, and number.
// Namespace carries the full path-segment chain before the repo
// (a single "owner" for GitHub, possibly a "group/sub/sub" chain for
// GitLab subgroups).
type PRReference struct {
	Forge     string // "github" | "gitlab"
	Namespace string // "owner" or "group/sub/..."
	Repo      string
	Number    int
}

// Project returns "namespace/repo" suitable for passing as gh --repo or
// glab -R.
func (r PRReference) Project() string {
	if r.Namespace == "" {
		return r.Repo
	}
	return r.Namespace + "/" + r.Repo
}

// ErrUnsupportedForge is returned by every nullForge operation. Callers
// should surface it to the user as "this remote isn't a supported
// forge" rather than dispatching to a binary we don't have.
var ErrUnsupportedForge = errors.New("forge integration is not available for this remote")

// nullForge is the sentinel returned by Core.forgeFor when origin URL
// classification yields an unsupported host. Every operation returns
// ErrUnsupportedForge so callers can branch-free dispatch.
type nullForge struct{}

func (nullForge) ID() string         { return "" }
func (nullForge) BinaryName() string { return "" }

func (nullForge) ListOpenPRs(string, string) ([]GitPR, error) {
	return nil, ErrUnsupportedForge
}

func (nullForge) CreatePR(string, string, string, bool) (string, error) {
	return "", ErrUnsupportedForge
}

func (nullForge) ViewPR(string, string, int) (PRMetadata, error) {
	return PRMetadata{}, ErrUnsupportedForge
}

func (nullForge) Diff(string, string, int) (string, error) {
	return "", ErrUnsupportedForge
}

// PRAnchorScheme is the URI scheme used for the project-row anchor we
// generate when a PR/MR thread has no local clone matching its repo.
// The anchor is opaque — it is stored as Project.Path and used as a
// uniqueness key, never re-parsed. Use BuildPRAnchor to construct one.
const PRAnchorScheme = "pr://"

// BuildPRAnchor constructs a "pr://forge/namespace/repo" pseudo-URI
// for the project-row of a PR/MR thread that has no matching local
// clone. The forge prefix makes the anchor self-describing without
// requiring callers to re-classify the namespace later.
func BuildPRAnchor(forge, namespace, repo string) string {
	return fmt.Sprintf("%s%s/%s/%s", PRAnchorScheme, forge, namespace, repo)
}

// SplitProjectForForge separates "namespace/repo" with per-forge
// segment rules: github requires exactly two segments (owner/repo),
// gitlab accepts any N≥2 segments where everything before the last is
// the namespace (group/sub/.../repo).
//
// Each segment is also validated against safe-name rules — no leading
// dashes (would be misread as flags by shell-out targets), no `.` or
// `..` (path traversal in the pseudo-anchor), no control characters
// or whitespace. The CLI argv path itself is shell-safe (we never
// interpolate via a shell), but defense-in-depth keeps the values
// out of DB rows and logs in pathological shapes.
func SplitProjectForForge(forgeID, project string) (namespace, repo string, err error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", "", errors.New("project is required")
	}
	parts := strings.Split(project, "/")
	for _, p := range parts {
		if err := ValidateProjectSegment(p); err != nil {
			return "", "", fmt.Errorf("project %q: %w", project, err)
		}
	}

	switch forgeID {
	case "github":
		if len(parts) != 2 {
			return "", "", fmt.Errorf("github project must be in the form OWNER/REPO, got %q", project)
		}
		return parts[0], parts[1], nil
	case "gitlab":
		if len(parts) < 2 {
			return "", "", fmt.Errorf("gitlab project must be NAMESPACE/REPO (or longer for subgroups), got %q", project)
		}
		return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
	default:
		return "", "", fmt.Errorf("unsupported forge %q", forgeID)
	}
}

// ValidateProjectSegment enforces a conservative char class on a
// single namespace/repo path segment. Rejects empty, `.`/`..`, leading
// dash, whitespace, and control characters. The accepted set covers
// all real github / gitlab owner / namespace / repo names.
func ValidateProjectSegment(seg string) error {
	if seg == "" {
		return errors.New("segment is empty")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("segment %q is not allowed", seg)
	}
	if seg[0] == '-' {
		return fmt.Errorf("segment %q must not start with '-'", seg)
	}
	for _, r := range seg {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("segment %q contains a control or whitespace character", seg)
		}
	}
	return nil
}

// NormalizePRState maps a forge-native PR/MR state to a canonical
// lowercase vocabulary: "open", "closed", "merged", "locked", or "".
// Both gh ("OPEN") and glab ("opened") map onto "open"; the rest
// already align after lowercasing.
func NormalizePRState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open", "opened":
		return "open"
	case "closed":
		return "closed"
	case "merged":
		return "merged"
	case "locked":
		return "locked"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}
