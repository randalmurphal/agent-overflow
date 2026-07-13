package git

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParsePRURL parses the URL shape returned by the GitHub and GitLab
// CreatePR implementations into the coordinates used by forge reads.
func ParsePRURL(raw string) (PRReference, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return PRReference{}, fmt.Errorf("parse PR URL: %w", err)
	}
	validScheme := parsed.Scheme == "https" || parsed.Scheme == "http"
	if !validScheme || parsed.Host == "" {
		return PRReference{}, fmt.Errorf("parse PR URL: absolute HTTP(S) URL is required")
	}
	segments, err := prURLPathSegments(parsed.EscapedPath())
	if err != nil {
		return PRReference{}, err
	}

	var ref PRReference
	switch {
	case len(segments) == 4 && segments[2] == "pull":
		ref.Forge = "github"
		ref.Namespace = segments[0]
		ref.Repo = segments[1]
		ref.Number, err = parsePRURLNumber(segments[3])
	case len(segments) >= 5 && segments[len(segments)-3] == "-" && segments[len(segments)-2] == "merge_requests":
		ref.Forge = "gitlab"
		project := strings.Join(segments[:len(segments)-3], "/")
		ref.Namespace, ref.Repo, err = SplitProjectForForge(ref.Forge, project)
		if err == nil {
			ref.Number, err = parsePRURLNumber(segments[len(segments)-1])
		}
	default:
		return PRReference{}, fmt.Errorf("parse PR URL: unsupported pull request URL path")
	}
	if err != nil {
		return PRReference{}, fmt.Errorf("parse PR URL: %w", err)
	}
	namespace, repo, err := SplitProjectForForge(ref.Forge, ref.Project())
	if err != nil {
		return PRReference{}, fmt.Errorf("parse PR URL: %w", err)
	}
	ref.Namespace = namespace
	ref.Repo = repo
	return ref, nil
}

func prURLPathSegments(escapedPath string) ([]string, error) {
	rawSegments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(rawSegments) == 1 && rawSegments[0] == "" {
		return nil, fmt.Errorf("parse PR URL: path is empty")
	}
	segments := make([]string, len(rawSegments))
	for index, segment := range rawSegments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return nil, fmt.Errorf("parse PR URL: decode path segment: %w", err)
		}
		segments[index] = decoded
	}
	return segments, nil
}

func parsePRURLNumber(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("pull request number must be positive, got %q", value)
	}
	return number, nil
}
