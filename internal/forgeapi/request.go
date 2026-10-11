package forgeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// prepare reads the credential, builds the request and passes the gate.
func (c *Client) prepare(ctx context.Context, r Request, graphQL bool) (*exchange, error) {
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := c.credential(ctx); err != nil {
		return nil, err
	}
	req, replayable, err := c.build(ctx, r, graphQL)
	if err != nil {
		return nil, err
	}
	c.credMu.RLock()
	fp := c.fp
	c.credMu.RUnlock()
	ex := &exchange{req: req, graphQL: graphQL, body: !replayable}
	ex.key = gateKey{host: c.host, pool: c.pool(graphQL), fingerprint: fp}
	ex.lease, err = c.svc.gates.check(ex.key, IsInteractive(ctx), c.svc.now())
	if err != nil {
		return nil, err
	}
	return ex, nil
}

// build turns r into an http.Request on the host's bases. replayable
// reports whether the call can be built again after a 401.
func (c *Client) build(ctx context.Context, r Request, graphQL bool) (*http.Request, bool, error) {
	c.credMu.RLock()
	rest, gql := c.rest, c.graphQL
	c.credMu.RUnlock()
	method := methodOf(r)
	var target *url.URL
	var virtualHost string
	switch {
	case graphQL:
		target = gql
		if c.svc.isolated != nil {
			virtualHost = c.host
		}
	default:
		parsed, err := url.Parse(r.Path)
		if err != nil {
			return nil, false, fmt.Errorf("forgeapi: request path %q: %w", r.Path, err)
		}
		if parsed.IsAbs() || parsed.Host != "" {
			if !r.Attachment {
				return nil, false, ErrAbsoluteURL
			}
			if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
				return nil, false, fmt.Errorf("forgeapi: attachment URL %s is not https", RedactURL(parsed))
			}
			target = c.svc.isolatedAbsolute(c.forge, parsed)
			break
		}
		if strings.HasPrefix(parsed.Path, "/") || hasDotSegment(parsed.Path) {
			return nil, false, fmt.Errorf("forgeapi: request path %q must be relative to the API base", r.Path)
		}
		target = rest.ResolveReference(parsed)
		if c.svc.isolated != nil {
			virtualHost = c.host
		}
	}
	if len(r.Query) > 0 {
		query := target.Query()
		for k, v := range r.Query {
			query[k] = append([]string(nil), v...)
		}
		target.RawQuery = query.Encode()
	}

	// A JSON body is encoded again for a retry; a reader is spent by the
	// first send.
	var body io.Reader
	replayable := true
	contentType := ""
	switch b := r.Body.(type) {
	case nil:
	case io.Reader:
		body = b
		replayable = false
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return nil, false, fmt.Errorf("forgeapi: encode request body: %w", err)
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, clientKeyCtx{}, c), method, target.String(), body)
	if err != nil {
		return nil, false, fmt.Errorf("forgeapi: build request: %w", err)
	}
	if virtualHost != "" {
		req.Host = virtualHost
	}
	req.Header.Set("User-Agent", c.svc.userAgent)
	switch c.forge {
	case ForgeGitHub:
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	default:
		req.Header.Set("Accept", "application/json")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, values := range r.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "Private-Token", "Host", "User-Agent":
			return nil, false, fmt.Errorf("forgeapi: a request may not set %s", http.CanonicalHeaderKey(name))
		}
		req.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return req, replayable, nil
}

func methodOf(r Request) string {
	if r.Method == "" {
		return http.MethodGet
	}
	return strings.ToUpper(r.Method)
}

func hasDotSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." || segment == "." {
			return true
		}
	}
	return false
}

// isolatedAbsolute rewrites an attachment URL onto the fake's base in an
// isolated boot; otherwise it is the URL itself.
func (s *Service) isolatedAbsolute(forge string, u *url.URL) *url.URL {
	if s.isolated == nil {
		return u
	}
	out := *s.isolated
	prefix := "/" + forge + "/absolute/" + u.Host
	out.Path = s.isolated.Path + prefix + u.Path
	out.RawPath = ""
	if u.RawPath != "" {
		out.RawPath = s.isolated.EscapedPath() + prefix + u.EscapedPath()
	}
	out.RawQuery = u.RawQuery
	return &out
}

// redact renders u for an error. An isolated boot's rewritten attachment
// URL is judged by the attachment's own host, which is never the fake's.
func (c *Client) redact(u *url.URL) string {
	if iso := c.svc.isolated; iso != nil && strings.EqualFold(u.Host, iso.Host) &&
		strings.HasPrefix(u.Path, iso.Path+"/"+c.forge+"/absolute/") {
		return RedactURL(u)
	}
	c.credMu.RLock()
	hosts := []string{c.host}
	if c.rest != nil {
		hosts = append(hosts, c.rest.Host)
	}
	if c.graphQL != nil {
		hosts = append(hosts, c.graphQL.Host)
	}
	c.credMu.RUnlock()
	return RedactURL(u, hosts...)
}
