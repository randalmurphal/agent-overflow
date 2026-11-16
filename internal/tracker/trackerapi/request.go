package trackerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is one call.
type Request struct {
	// Method is GET, HEAD, POST, PUT, PATCH or DELETE; empty means GET.
	// Only a GET or HEAD follows a redirect. A 3xx answering any other
	// method fails the call with a *StatusError: a write is never sent on.
	Method string
	// Path is relative to Options.BaseURL ("rest/api/3/issue/WEB-1"), with
	// an optional query, and never starts with "/" or holds a dot segment.
	// It is a full URL only with Absolute set. Tracker text in it is the
	// caller's to escape.
	Path  string
	Query url.Values // added to Path's query, replacing keys it repeats
	// Header carries headers such as Accept or Range. Authorization,
	// Proxy-Authorization, Cookie, Host, User-Agent, Content-Type and
	// Content-Length are refused, and so are Idempotency-Key and
	// X-Idempotency-Key, which would let net/http send a write again.
	Header http.Header
	// Body is JSON-encoded, except a []byte, which is sent as is with
	// ContentType. An io.Reader is refused: Multipart turns an upload into
	// bytes. The caller does not change a []byte until the call returns.
	// GET and HEAD carry no body.
	Body any
	// ContentType is the body's type: required for a []byte body,
	// application/json when empty for any other.
	ContentType string
	// ReadOnly marks a POST that only reads (a search, a GraphQL query):
	// it is retried like a GET. Refused on any other method.
	ReadOnly bool
	// Absolute marks Path as a full URL the tracker handed out (an
	// attachment's content). It is https unless its host is a loopback IP
	// address. Only a URL on the base origin receives the credential.
	Absolute bool
	// Transfer marks an upload or download: no total timeout, a stall
	// timeout and a header timeout instead (transfer.go). Refused with a
	// Timeout.
	Transfer bool
	// Timeout bounds the whole call, retries and waits included; zero
	// takes Options.ReadTimeout. A shorter context deadline wins.
	Timeout time.Duration
	// MaxBodyBytes bounds the answer body Do and JSON read; zero takes
	// Options.MaxBodyBytes. Stream takes its limit as an argument and
	// refuses one here. An error answer's body is read up to this bound or
	// 64 KiB, whichever is less, and kept as far as it was read: the
	// status is the answer, never a *BodyTooLargeError.
	MaxBodyBytes int64
}

// Response is a tracker's answer.
type Response struct {
	Status int
	Header http.Header
	// Body is the whole answer for Do and JSON; nil for Stream.
	Body []byte
	// scrub is what Scrub removes; nil when the request carried no
	// credential.
	scrub *scrubRef
}

// call is one prepared Request, sent by each attempt.
type call struct {
	method      string
	target      *url.URL
	body        []byte
	hasBody     bool
	contentType string
	header      http.Header
	read        bool // retried on a connection failure and a 503
	transfer    bool
	timeout     time.Duration
	limit       int64 // the body bound of Do and JSON, or Stream's limit
	redirects   redirectMode
	accept      string // Accept when the caller sets none
	// committed is set once Stream wrote to its destination, after which
	// the call is never retried.
	committed bool
}

// prepare checks r and builds what every attempt sends. Its errors are
// the caller's and nothing is sent.
func (c *Client) prepare(r Request) (*call, error) {
	k := &call{method: strings.ToUpper(r.Method), timeout: r.Timeout, transfer: r.Transfer}
	if k.method == "" {
		k.method = http.MethodGet
	}
	switch k.method {
	case http.MethodGet, http.MethodHead:
		k.read = true
		k.redirects = followRedirect
	case http.MethodPost:
		k.read = r.ReadOnly
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, fmt.Errorf("method %q is not one a tracker call uses", r.Method)
	}
	switch {
	case r.ReadOnly && k.method != http.MethodPost:
		return nil, fmt.Errorf("only a POST can be ReadOnly, not a %s", k.method)
	case r.Timeout < 0:
		return nil, errors.New("the request timeout is negative")
	case r.Transfer && r.Timeout != 0:
		return nil, errors.New("a Transfer request has no total timeout, so it takes no Timeout")
	case r.MaxBodyBytes < 0:
		return nil, errors.New("the request MaxBodyBytes is negative")
	}
	if k.timeout == 0 {
		k.timeout = c.readTimeout
	}
	k.limit = r.MaxBodyBytes
	if k.limit == 0 {
		k.limit = c.maxBody
	}
	target, err := c.resolve(r)
	if err != nil {
		return nil, err
	}
	k.target = target
	if err := k.setBody(r); err != nil {
		return nil, err
	}
	k.header = make(http.Header, len(r.Header))
	for name, values := range r.Header {
		if err := checkHeader(name, values); err != nil {
			return nil, err
		}
		k.header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return k, nil
}

// resolve turns r's path into the URL every attempt requests.
func (c *Client) resolve(r Request) (*url.URL, error) {
	parsed, err := url.Parse(r.Path)
	if err != nil {
		// Not quoted: an Absolute path is the tracker's and can be signed.
		return nil, errors.New("request path is not a URL")
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	if r.Absolute {
		switch {
		case !parsed.IsAbs() || parsed.Host == "":
			return nil, errors.New("an Absolute request path is not a full URL")
		case parsed.User != nil:
			return nil, errors.New("an absolute request URL carries user info")
		}
		if err := allowedURL(parsed); err != nil {
			return nil, err
		}
		return mergeQuery(parsed, r.Query), nil
	}
	switch {
	case parsed.IsAbs() || parsed.Host != "":
		return nil, errors.New("a full URL as the request path needs Absolute")
	case strings.HasPrefix(parsed.Path, "/") || hasDotSegment(parsed.Path):
		return nil, fmt.Errorf("request path %q must be relative to the base URL, without dot segments", parsed.Path)
	}
	return mergeQuery(c.base.ResolveReference(parsed), r.Query), nil
}

func mergeQuery(u *url.URL, add url.Values) *url.URL {
	if len(add) == 0 {
		return u
	}
	query := u.Query()
	for k, v := range add {
		query[k] = append([]string(nil), v...)
	}
	u.RawQuery = query.Encode()
	return u
}

func hasDotSegment(p string) bool {
	for segment := range strings.SplitSeq(p, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func (k *call) setBody(r Request) error {
	switch b := r.Body.(type) {
	case nil:
		if r.ContentType != "" {
			return errors.New("a content type without a body")
		}
		return nil
	case []byte:
		if r.ContentType == "" {
			return errors.New("a []byte body needs a ContentType")
		}
		k.body = b
	case io.Reader:
		// Encoded as JSON it would send "{}"; read here it would block
		// before the call can be cancelled.
		return errors.New("an io.Reader body is refused; pass its bytes")
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("encode the request body: %w", err)
		}
		k.body = encoded
	}
	if k.method == http.MethodGet || k.method == http.MethodHead {
		return fmt.Errorf("a %s carries no body", k.method)
	}
	k.hasBody = true
	k.contentType = r.ContentType
	if k.contentType == "" {
		k.contentType = "application/json"
	}
	if hasControl(k.contentType) {
		return errors.New("the content type holds a control character")
	}
	return nil
}

func checkHeader(name string, values []string) error {
	// The transport owns these. An idempotency key makes net/http resend
	// a write whose connection fails, and no tracker here honors one.
	switch canonical := http.CanonicalHeaderKey(name); canonical {
	case "Authorization", "Proxy-Authorization", "Cookie", "Host", "User-Agent", "Content-Type", "Content-Length",
		"Idempotency-Key", "X-Idempotency-Key":
		return fmt.Errorf("a request may not set %s", canonical)
	}
	if name == "" || strings.ContainsFunc(name, func(r rune) bool { return !isTokenChar(r) }) {
		return fmt.Errorf("header name %q is not a token", name)
	}
	for _, v := range values {
		if strings.ContainsFunc(v, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) {
			return fmt.Errorf("header %s holds a control character", name)
		}
	}
	return nil
}

// isTokenChar reports a character RFC 9110 allows in a header name.
func isTokenChar(r rune) bool {
	return r < 0x7f && r > 0x20 && !strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r)
}

// request builds one hop's http.Request to target on ctx, without a
// credential. It has no GetBody, so net/http never sends its body again:
// a request with a body is replayed only through GetBody. A transfer's
// body reports its progress to st.
func (k *call) request(ctx context.Context, target *url.URL, st *stall, userAgent string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, k.method, "", nil)
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	u := *target
	req.URL = &u
	switch {
	case !k.hasBody:
	case len(k.body) == 0:
		req.Body = http.NoBody
	case st != nil:
		req.Body, req.ContentLength = st.upload(k.body), int64(len(k.body))
	default:
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(k.body)), int64(len(k.body))
	}
	for name, values := range k.header {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("User-Agent", userAgent)
	if k.accept != "" && req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", k.accept)
	}
	if k.hasBody {
		req.Header.Set("Content-Type", k.contentType)
	}
	return req, nil
}

// origin is a URL's scheme, host and port, the unit a credential is
// confined to.
type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: u.Port()}
	if o.port == "" {
		switch o.scheme {
		case "https":
			o.port = "443"
		case "http":
			o.port = "80"
		}
	}
	return o
}

// allowedURL accepts an https URL, or an http one whose host is a loopback
// IP address, so a test server is reachable and a mistyped scheme is not.
func allowedURL(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("%s is not https", redact(u))
}

// redact renders u for an error: scheme, host and path. User info, query
// and fragment are dropped, since a query (a signed download URL's) can
// be a credential in all but name.
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	out := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	return out.String()
}

// parseCause drops the *url.Error wrapper, whose message quotes the whole
// URL, query included.
func parseCause(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}
