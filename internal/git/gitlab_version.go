package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// The oldest GitLab release the app supports is gitlabSupportedFloor,
// read from GET /metadata (GitLab 15.2 and later) once per host per
// gitlabCacheTTL. Everything the app uses is in that release, so a server
// at or past it has it all and an older one is refused with one sentence
// naming both releases. No decision reads the schema: GitLab 18.9 and
// later refuse introspection in an ordinary document ("Field '__type'
// doesn't exist on type 'Query'").

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

// gitlabSupportedFloor is the oldest GitLab release the app supports.
var gitlabSupportedFloor = gitlabVersion{18, 0}

// gitlabServer is what a Details read or a write knows of the host's
// release: the version, or why it could not be read.
type gitlabServer struct {
	version gitlabVersion
	err     error
}

// supported is nil for a server at or past gitlabSupportedFloor; an
// older one is refused, naming its release and the floor, and a failed
// version read is its error.
func (s gitlabServer) supported(host string) error {
	if s.err != nil {
		return fmt.Errorf("read the GitLab server's version: %w", s.err)
	}
	if !s.version.atLeast(gitlabSupportedFloor) {
		return &ForgeActionError{Kind: ForgeErrorRefused, Message: fmt.Sprintf("GitLab on %s runs %s; GitLab %s or later is needed.", host, s.version, gitlabSupportedFloor)}
	}
	return nil
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

// writeClient is the client for a write to ref, refused before anything
// is sent unless the host's server is supported (gitlabServer.supported).
// A failed version read is a write not sent, so its caller never reports
// it as one the forge may have applied.
func (f *gitlabForge) writeClient(ctx context.Context, ref PRReference) (*forgeapi.Client, error) {
	client, _, err := f.writeServer(ctx, ref)
	return client, err
}

// writeServer is writeClient with the server's release, for a write whose
// request depends on it.
func (f *gitlabForge) writeServer(ctx context.Context, ref PRReference) (*forgeapi.Client, gitlabVersion, error) {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return nil, gitlabVersion{}, err
	}
	server := f.server(ctx, client, ref.Host)
	if err := server.supported(ref.Host); err != nil {
		return nil, gitlabVersion{}, &forgeapi.NotSentError{Err: err}
	}
	return client, server.version, nil
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
