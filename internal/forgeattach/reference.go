// Package forgeattach resolves the media a PR/MR body or review comment
// references on its forge, so the review pane can show it with the
// user's own gh / glab login as the credential.
//
// Three pieces and no I/O. ParseReference turns a markdown href into the
// request one forge CLI understands, Classify decides what the returned
// bytes actually are by signature, and Cache holds them for the ticketed
// byte route that serves them. The fetch itself belongs to internal/git,
// which owns every gh / glab subprocess.
package forgeattach

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxBytes caps one attachment body. Both forges refuse uploads well
	// under this, so a response larger than it is a wrong target rather
	// than a real attachment.
	MaxBytes int64 = 100 << 20

	// DefaultCacheBytes bounds the in-memory cache behind the byte route.
	// Above MaxBytes so one large video cannot evict everything a body
	// referenced alongside it.
	DefaultCacheBytes int64 = 128 << 20

	// DefaultTTL is how long fetched bytes stay resolvable. Long enough
	// to cover a reader scrolling back through a review, short enough
	// that a closed pane does not pin a video for the session.
	DefaultTTL = 10 * time.Minute
)

// Target is one attachment reference resolved to the request its forge
// CLI takes.
type Target struct {
	// Forge is "github" or "gitlab".
	Forge string
	// Request is what the CLI is asked for. An absolute https URL for
	// GitHub, because `gh api` passes an argument containing "://"
	// through as the request URL. A REST path for GitLab:
	// projects/<escaped project>/uploads/<secret>/<name>, the 17.4+
	// "download an uploaded file by secret" endpoint.
	Request string
	// Filename is the download and display name: the last path segment
	// percent-decoded where the reference carries one, else the
	// attachment id. The bytes decide the type, never this.
	Filename string
}

var (
	errNotGitHubAttachment = errors.New("not a GitHub attachment URL")
	errNotGitLabUpload     = errors.New("not a GitLab upload URL")
)

// gitLabUploadSecret is the 32 lowercase hex chars GitLab puts between
// /uploads/ and the file name. Matching it exactly is what keeps this
// parser from treating an arbitrary repository path as an upload.
var gitLabUploadSecret = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ParseReference resolves one href as written in markdown.
//
// project is the PR/MR's own project ("namespace/repo"), used for the
// GitLab references that are relative to the repository they were
// written in. It is ignored for GitHub, whose attachment URLs are always
// absolute and always carry their own identity.
func ParseReference(forge, project, href string) (Target, error) {
	href = strings.TrimFunc(href, func(r rune) bool { return r <= ' ' })
	if href == "" {
		return Target{}, errors.New("attachment reference is empty")
	}
	for _, r := range href {
		if r < 0x20 || r == 0x7f {
			return Target{}, errors.New("attachment reference contains a control character")
		}
	}
	switch forge {
	case "github":
		return parseGitHubReference(href)
	case "gitlab":
		return parseGitLabReference(project, href)
	default:
		return Target{}, fmt.Errorf("unsupported forge %q", forge)
	}
}

func parseGitHubReference(href string) (Target, error) {
	u, err := url.Parse(href)
	if err != nil {
		return Target{}, errNotGitHubAttachment
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return Target{}, errNotGitHubAttachment
	}
	// A port would mean a host we did not check. Both attachment hosts
	// are served on the default one.
	if u.Port() != "" || u.User != nil {
		return Target{}, errNotGitHubAttachment
	}
	host := strings.ToLower(u.Hostname())
	escaped, decoded, err := pathSegments(u.EscapedPath())
	if err != nil {
		return Target{}, errNotGitHubAttachment
	}

	switch host {
	case "private-user-images.githubusercontent.com":
		// The short-lived signed URL GitHub redirects an asset to. Its
		// shape is GitHub's to change, so nothing here reads it beyond
		// requiring a path; the QUERY is the whole point and is kept.
		if len(escaped) == 0 {
			return Target{}, errNotGitHubAttachment
		}
		return Target{
			Forge:    "github",
			Request:  "https://" + host + "/" + strings.Join(escaped, "/") + queryOf(u),
			Filename: decoded[len(decoded)-1],
		}, nil
	case "github.com":
		name, ok := gitHubAttachmentName(decoded)
		if !ok {
			return Target{}, errNotGitHubAttachment
		}
		return Target{
			Forge:    "github",
			Request:  "https://github.com/" + strings.Join(escaped, "/"),
			Filename: name,
		}, nil
	default:
		return Target{}, errNotGitHubAttachment
	}
}

// gitHubAttachmentName matches the four github.com attachment shapes and
// answers the name to save the bytes under. The `files/` forms carry a
// real name; the `assets/` forms carry only an opaque id, and the id is
// then the honest answer — the response bytes decide the type, so
// inventing an extension here would only be a guess a client might
// believe.
func gitHubAttachmentName(segments []string) (string, bool) {
	switch {
	case len(segments) == 3 && segments[0] == "user-attachments" && segments[1] == "assets":
		return segments[2], true
	case len(segments) == 4 && segments[0] == "user-attachments" && segments[1] == "files":
		return segments[3], true
	case len(segments) == 5 && (segments[2] == "assets" || segments[2] == "files"):
		return segments[4], true
	default:
		return "", false
	}
}

func parseGitLabReference(project, href string) (Target, error) {
	var (
		escaped []string
		decoded []string
		err     error
	)
	if strings.Contains(href, "://") {
		u, parseErr := url.Parse(href)
		if parseErr != nil {
			return Target{}, errNotGitLabUpload
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
		default:
			return Target{}, errNotGitLabUpload
		}
		if u.Hostname() == "" || u.User != nil {
			return Target{}, errNotGitLabUpload
		}
		escaped, decoded, err = pathSegments(u.EscapedPath())
	} else {
		// A project-relative reference as GitLab writes it in markdown.
		// The leading slash is required: a bare relative path is not
		// something GitLab emits, and accepting one would mean guessing
		// what it was relative to.
		if !strings.HasPrefix(href, "/") {
			return Target{}, errNotGitLabUpload
		}
		path := href
		if cut := strings.IndexAny(path, "?#"); cut >= 0 {
			path = path[:cut]
		}
		escaped, decoded, err = pathSegments(path)
	}
	if err != nil {
		return Target{}, errNotGitLabUpload
	}

	// Anchored at the END rather than searched for: the last three
	// segments are uploads/<secret>/<name> and everything before them
	// identifies the project, so a repository that is itself called
	// "uploads" cannot shift the match.
	if len(escaped) < 3 || escaped[len(escaped)-3] != "uploads" {
		return Target{}, errNotGitLabUpload
	}
	secret := escaped[len(escaped)-2]
	if !gitLabUploadSecret.MatchString(secret) {
		return Target{}, errNotGitLabUpload
	}
	name := escaped[len(escaped)-1]
	filename := decoded[len(decoded)-1]
	prefix := decoded[:len(decoded)-3]

	target := project
	switch {
	case len(prefix) == 0:
		// Relative to the repository the comment was written in.
	case len(prefix) == 3 && prefix[0] == "-" && prefix[1] == "project" && isDigits(prefix[2]):
		target = prefix[2]
	default:
		// An absolute repository URL. GitLab renders a "/-/" separator
		// before some sub-paths; it is routing, not part of the project.
		kept := make([]string, 0, len(prefix))
		for _, segment := range prefix {
			if segment != "-" {
				kept = append(kept, segment)
			}
		}
		if len(kept) < 2 {
			return Target{}, errNotGitLabUpload
		}
		target = strings.Join(kept, "/")
	}
	if target == "" {
		return Target{}, errors.New("GitLab upload reference is relative and no project is known")
	}

	return Target{
		Forge: "gitlab",
		// The name keeps its ESCAPED spelling: it is a path segment of
		// the request, and re-escaping a decoded one would double-encode
		// every literal percent GitLab stored.
		Request:  "projects/" + url.PathEscape(target) + "/uploads/" + secret + "/" + name,
		Filename: filename,
	}, nil
}

// pathSegments splits an escaped URL path and returns both spellings of
// every segment, refusing anything that could not be a file reference:
// an empty segment, a relative one, an undecodable escape, or a decoded
// value carrying a separator or a control character. Rejecting here is
// what lets the callers join segments back into a request without
// re-checking them.
func pathSegments(escapedPath string) (escaped, decoded []string, err error) {
	trimmed := strings.TrimPrefix(escapedPath, "/")
	if trimmed == "" {
		return nil, nil, errors.New("path is empty")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, nil, fmt.Errorf("path segment %q is not a file reference", segment)
		}
		plain, unescapeErr := url.PathUnescape(segment)
		if unescapeErr != nil {
			return nil, nil, unescapeErr
		}
		if plain == "" || plain == "." || plain == ".." ||
			strings.ContainsAny(plain, "/\\") {
			return nil, nil, fmt.Errorf("path segment %q is not a file reference", plain)
		}
		for _, r := range plain {
			if r < 0x20 || r == 0x7f {
				return nil, nil, errors.New("path segment contains a control character")
			}
		}
		escaped = append(escaped, segment)
		decoded = append(decoded, plain)
	}
	return escaped, decoded, nil
}

func queryOf(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CacheKey is the dedupe key for one PR's reference to one href, so a
// second render of the same body reuses the bytes instead of spawning
// the forge CLI again.
func CacheKey(forge, project string, number int, href string) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s", forge, project, number,
		strings.TrimFunc(href, func(r rune) bool { return r <= ' ' }))
}
