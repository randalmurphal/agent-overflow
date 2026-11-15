package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// What a GitLab server supports is decided by its release, read from
// GET /metadata (GitLab 15.2 and later) once per host per gitlabCacheTTL,
// and the release GitLab's GraphQL reference first documents each
// capability in (docs.gitlab.com/<version>/api/graphql/reference). No
// decision reads the schema: GitLab 18.9 and later refuse introspection
// in an ordinary document ("Field '__type' doesn't exist on type
// 'Query'").

// gitlabVersion is a GitLab release, major.minor.
type gitlabVersion struct{ major, minor int }

func (v gitlabVersion) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }

// atLeast reports whether v is release o or later.
func (v gitlabVersion) atLeast(o gitlabVersion) bool {
	return v.major > o.major || (v.major == o.major && v.minor >= o.minor)
}

// parseGitLabVersion reads the release of a /metadata version
// ("17.9.2-ee", "18.0.0-pre").
func parseGitLabVersion(s string) (gitlabVersion, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 3)
	if len(parts) < 2 {
		return gitlabVersion{}, fmt.Errorf("GitLab answered version %q, not major.minor", s)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil || major <= 0 || minor < 0 {
		return gitlabVersion{}, fmt.Errorf("GitLab answered version %q, not major.minor", s)
	}
	return gitlabVersion{major, minor}, nil
}

// gitlabSupportedFloor is the oldest GitLab release the app supports. A
// review submit to an older server is refused before any write, since
// its draft notes post something other than what the pane shows.
var gitlabSupportedFloor = gitlabVersion{16, 0}

// gitlabCapability is a GitLab feature the app uses, the release that
// introduced it, and the control it decides (empty for one that only
// picks a document).
type gitlabCapability struct {
	control string
	since   gitlabVersion
}

// The management capabilities, each first documented in the GraphQL
// reference of since. The re-review mutation and the set mutations'
// operationMode are in the 16.0 reference, the oldest one read.
var (
	gitlabCapRereview     = gitlabCapability{control: "re-requesting a review", since: gitlabVersion{16, 0}}
	gitlabCapSetReviewers = gitlabCapability{control: "changing reviewers", since: gitlabVersion{16, 0}}
	gitlabCapSetAssignees = gitlabCapability{control: "changing assignees", since: gitlabVersion{16, 0}}
	gitlabCapSetLabels    = gitlabCapability{control: "changing labels", since: gitlabVersion{16, 0}}
	// gitlabCapChecks is MergeRequest.mergeabilityChecks (PRManageChecks).
	gitlabCapChecks = gitlabCapability{since: gitlabVersion{16, 5}}
	// gitlabCapOverride is mergeRequestUpdate's overrideRequestedChanges.
	gitlabCapOverride = gitlabCapability{control: "bypassing requested changes", since: gitlabVersion{17, 9}}
)

// gitlabServer is what a Details read or a write knows of the host's
// release: the version, or why it could not be read.
type gitlabServer struct {
	version gitlabVersion
	err     error
}

// has reports whether the server is known to support c.
func (s gitlabServer) has(c gitlabCapability) bool {
	return s.err == nil && s.version.atLeast(c.since)
}

// lacking is the sentence for the controls the server does not support
// (or whose support is unknown), or empty for none.
func (s gitlabServer) lacking(controls []gitlabCapability) string {
	if len(controls) == 0 {
		return ""
	}
	if s.err != nil {
		names := make([]string, 0, len(controls))
		for _, c := range controls {
			names = append(names, c.control)
		}
		return fmt.Sprintf("The GitLab server's version could not be read, so %s %s not offered for now.", joinClauses(names), map[bool]string{true: "is", false: "are"}[len(names) == 1])
	}
	needs := make([]string, 0, len(controls))
	for _, c := range controls {
		needs = append(needs, fmt.Sprintf("%s needs GitLab %s", c.control, c.since))
	}
	return fmt.Sprintf("This GitLab server runs %s: %s.", s.version, joinClauses(needs))
}

// refuse is the write of c on a server without it: refused before it is
// sent, naming the release it needs, or the version read's failure.
func (s gitlabServer) refuse(ref PRReference, c gitlabCapability) error {
	if s.err != nil {
		return s.err
	}
	return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab on %s runs %s; %s needs GitLab %s.", ref.Host, s.version, c.control, c.since)}
}

// server is host's release: cached for gitlabCacheTTL, else read from
// GET /metadata. A failed read is not cached.
func (f *gitlabForge) server(ctx context.Context, client *forgeapi.Client, host string) gitlabServer {
	key := strings.ToLower(host)
	if version, ok := f.versions.get(key, f.core.nowFn()); ok {
		return gitlabServer{version: version}
	}
	var raw struct {
		Version string `json:"version"`
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Path: "metadata"}, &raw); err != nil {
		return gitlabServer{err: err}
	}
	version, err := parseGitLabVersion(raw.Version)
	if err != nil {
		return gitlabServer{err: err}
	}
	f.versions.put(key, version, f.core.nowFn())
	return gitlabServer{version: version}
}

// requireSupported refuses a server older than gitlabSupportedFloor,
// naming its release, and returns a failed version read as it is.
func (f *gitlabForge) requireSupported(ctx context.Context, client *forgeapi.Client, ref PRReference, what string) error {
	s := f.server(ctx, client, ref.Host)
	if s.err != nil {
		return s.err
	}
	if !s.version.atLeast(gitlabSupportedFloor) {
		return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab on %s runs %s; %s from here needs GitLab %s or later. Do it on GitLab.", ref.Host, s.version, what, gitlabSupportedFloor)}
	}
	return nil
}

// gitlabSchemaLacks reports whether err is GitLab refusing a document at
// validation for naming element, a field ("Field 'x' doesn't exist on
// type 'T'") or an argument or input field ("... doesn't accept argument
// 'x'"), that its schema does not have. A GraphQL server validates the
// whole document before running any of it, so a refused document did
// nothing.
func gitlabSchemaLacks(err error, element string) bool {
	gql, ok := errors.AsType[*forgeapi.GraphQLError](err)
	if !ok || gql.ProducedData() {
		return false
	}
	quoted := "'" + element + "'"
	for _, problem := range gql.Problems {
		if strings.Contains(problem.Message, "Field "+quoted+" doesn't exist on type") || strings.Contains(problem.Message, "doesn't accept argument "+quoted) {
			return true
		}
	}
	return false
}
