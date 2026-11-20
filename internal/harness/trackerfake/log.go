package trackerfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// maxLogged bounds the request log: the oldest entries go first and
// RequestLog.Dropped counts them.
const maxLogged = 4096

// maxLoggedMember bounds each member an entry keeps, in bytes: a text
// (the path, operation and detail, each query key and value, each file
// name) is cut to it, and a JSON value (the body, the variables) over it
// is left out. Request.Omitted names each.
const maxLoggedMember = 64 << 10

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
	// Operation and Variables are a GraphQL request's.
	Operation string          `json:"operation,omitempty"`
	Variables json.RawMessage `json:"variables,omitempty"`
	// Body is a JSON request body. Body and Variables are re-encoded:
	// keys sorted, no space.
	Body json.RawMessage `json:"body,omitempty"`
	// Files are the file names of a multipart upload.
	Files []string `json:"files,omitempty"`
	// Omitted names each member cut or left out for being over 64 KiB,
	// and its size.
	Omitted []string `json:"omitted,omitempty"`
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
	r.Omitted = slices.Clone(r.Omitted)
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

// logEntry is what the log keeps of x's entry, and the only way an entry
// is made: every credential the engine knows or x presented is replaced
// in every text, in every decoded JSON string, number and key, and every
// member is bounded by maxLoggedMember. Callers hold mu.
func (e *Engine) logEntry(x *exchange) Request {
	return entryOf(x.entry, e.secrets(x))
}

func entryOf(in Request, secrets []string) Request {
	r := in.clone()
	b := &entryBuilder{secrets: secrets}
	r.Path = b.text("path", r.Path)
	r.Operation = b.text("operation", r.Operation)
	r.Detail = b.text("detail", r.Detail)
	if r.Query != nil {
		q := url.Values{}
		for k, vs := range r.Query {
			key := b.text("query key", k)
			for _, v := range vs {
				q[key] = append(q[key], b.text("query "+key, v))
			}
		}
		r.Query = q
	}
	for i := range r.Files {
		r.Files[i] = b.text("file name", r.Files[i])
	}
	r.Body = b.json("body", r.Body)
	r.Variables = b.json("variables", r.Variables)
	r.Omitted = b.omitted
	return r
}

type entryBuilder struct {
	secrets []string
	omitted []string
}

// text is s scrubbed and cut to the bound.
func (b *entryBuilder) text(what, s string) string {
	s = scrubText(s, b.secrets)
	if len(s) <= maxLoggedMember {
		return s
	}
	b.omitted = append(b.omitted, fmt.Sprintf("%s: %d bytes cut to %d", what, len(s), maxLoggedMember))
	cut := maxLoggedMember
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// json is raw decoded, scrubbed and encoded again, or nothing when that
// is over the bound.
func (b *entryBuilder) json(what string, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		b.omitted = append(b.omitted, fmt.Sprintf("%s: not JSON: %v", what, err))
		return nil
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b.scrubValue(v)); err != nil {
		b.omitted = append(b.omitted, fmt.Sprintf("%s: encode: %v", what, err))
		return nil
	}
	encoded := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(encoded) > maxLoggedMember {
		b.omitted = append(b.omitted, fmt.Sprintf("%s: %d bytes left out, over %d", what, len(encoded), maxLoggedMember))
		return nil
	}
	return encoded
}

func (b *entryBuilder) scrubValue(v any) any {
	switch v := v.(type) {
	case string:
		return scrubText(v, b.secrets)
	case json.Number:
		if s := scrubText(string(v), b.secrets); s != string(v) {
			return s
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = b.scrubValue(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[scrubText(k, b.secrets)] = b.scrubValue(x)
		}
		return out
	}
	return v
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
