package trackerfake

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxLogged bounds the request log: the oldest entries go first and
// RequestLog.Dropped counts them.
const maxLogged = 4096

// maxLoggedBody bounds the JSON body an entry records.
const maxLoggedBody = 64 << 10

// redacted replaces a credential wherever an entry would repeat it.
const redacted = "<redacted>"

// Request is one recorded request and how it was answered. It never
// holds a credential: the Authorization header is not recorded, and any
// text that repeats a credential the engine knows or the request
// presented has it replaced by "<redacted>".
type Request struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Origin Origin    `json:"origin"`
	// Tenant is the Jira site, or the Linear workspace's URL key, the
	// request's credential named; empty when it named none.
	Tenant string `json:"tenant,omitempty"`
	Method string `json:"method"`
	// Path is the request path, without its query.
	Path string `json:"path"`
	// Route names what answered: a REST route ("GET rest/api/3/issue/{issue}"),
	// a GraphQL operation, or "offline", "unauthorized" or "closed". Empty
	// when Unhandled.
	Route string     `json:"route,omitempty"`
	Query url.Values `json:"query,omitempty"`
	// Operation and Variables are a GraphQL request's, as sent.
	Operation string          `json:"operation,omitempty"`
	Variables json.RawMessage `json:"variables,omitempty"`
	// Body is a JSON request body, at most 64 KiB.
	Body json.RawMessage `json:"body,omitempty"`
	// Files are the file names of a multipart upload.
	Files []string `json:"files,omitempty"`
	// Status is the answer's; zero for a connection closed with no answer
	// and for a held request not yet answered.
	Status int `json:"status"`
	// Unhandled marks a request the fake does not implement; Detail says
	// what it lacks.
	Unhandled bool `json:"unhandled,omitempty"`
	// Fault is the fault control that answered, when one did.
	Fault FaultKind `json:"fault,omitempty"`
	// Held reports a request a hold is keeping; it is cleared, and
	// Status set, when the request is answered.
	Held   bool   `json:"held,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RequestLog is a window of the recorded requests.
type RequestLog struct {
	Requests []Request `json:"requests"`
	// Dropped counts requests the log bound evicted since the last Reset.
	Dropped int `json:"dropped"`
}

type requestLog struct {
	entries []Request
	seq     int
	dropped int
}

func (l *requestLog) reset() {
	l.entries, l.dropped = nil, 0
}

// add appends r, numbered, and returns it as stored.
func (l *requestLog) add(r Request) Request {
	l.seq++
	r.Seq = l.seq
	l.entries = append(l.entries, r)
	if over := len(l.entries) - maxLogged; over > 0 {
		l.entries = slices.Delete(l.entries, 0, over)
		l.dropped += over
	}
	return r
}

// update replaces the entry numbered r.Seq, if the log still holds it.
func (l *requestLog) update(r Request) {
	for i := len(l.entries) - 1; i >= 0; i-- {
		if l.entries[i].Seq == r.Seq {
			l.entries[i] = r
			return
		}
	}
}

// Requests returns the recorded requests numbered after since.
func (e *Engine) Requests(since int) RequestLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := RequestLog{Requests: []Request{}, Dropped: e.log.dropped}
	for _, r := range e.log.entries {
		if r.Seq > since {
			out.Requests = append(out.Requests, r.clone())
		}
	}
	return out
}

// Unhandled returns the recorded requests the fake did not implement.
func (e *Engine) Unhandled() []Request {
	var out []Request
	for _, r := range e.Requests(0).Requests {
		if r.Unhandled {
			out = append(out, r)
		}
	}
	return out
}

func (r Request) clone() Request {
	r.Query = cloneValues(r.Query)
	r.Variables = bytes.Clone(r.Variables)
	r.Body = bytes.Clone(r.Body)
	r.Files = slices.Clone(r.Files)
	return r
}

func cloneValues(v url.Values) url.Values {
	if v == nil {
		return nil
	}
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}

// scrubbed is r with every secret replaced wherever its text repeats one.
// A secret that does not appear leaves the text as it is.
func (r Request) scrubbed(secrets []string) Request {
	r = r.clone()
	scrub := func(s string) string { return scrubText(s, secrets) }
	r.Path, r.Detail, r.Operation = scrub(r.Path), scrub(r.Detail), scrub(r.Operation)
	for k, vs := range r.Query {
		for i := range vs {
			vs[i] = scrub(vs[i])
		}
		if sk := scrub(k); sk != k {
			delete(r.Query, k)
			r.Query[sk] = vs
		}
	}
	for i := range r.Files {
		r.Files[i] = scrub(r.Files[i])
	}
	r.Body = scrubJSON(r.Body, scrub)
	r.Variables = scrubJSON(r.Variables, scrub)
	return r
}

// scrubText is s with every secret replaced by the redacted marker.
func scrubText(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	return s
}

// scrubJSON scrubs a JSON value's text; a value whose scrubbed text is no
// longer JSON is replaced by the redacted marker as a string.
func scrubJSON(raw json.RawMessage, scrub func(string) string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	out := scrub(string(raw))
	if out == string(raw) {
		return raw
	}
	if !json.Valid([]byte(out)) {
		return json.RawMessage(`"` + redacted + `"`)
	}
	return json.RawMessage(out)
}
