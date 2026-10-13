package forgefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// `gh api` and `glab api`, the CLIs' own way to call the forge APIs, as
// the pstack forge script (assets/skills/pstack) runs them. A CLI call is
// answered by the same REST and GraphQL route tables as the HTTP mounts:
// the endpoint is the one positional, --hostname names the forge host
// (each CLI's default host when omitted), --method or a body picks the
// method, and --input - is the request body read from stdin. A GraphQL
// call (`gh api graphql`) carries its document and variables as fields:
// -f is a raw string, -F converts true, false, null and integers and reads
// @- from stdin. -H sets the request's Content-Type, the one header
// implemented; a write route refuses a body that is not application/json.
// A route lists input or paginate among its flags when it takes a body or
// answers --paginate; any other flag on it is refused.
//
// --paginate follows the forge's next-page signal and prints what the CLI
// prints: gh writes each page's JSON in turn, glab writes each page's
// array straight after the last (`[...][...]`, glab 1.117.0), so a caller
// reading one value slurps and adds the pages. A GraphQL --paginate
// follows the outermost connection's pageInfo through the document's
// $endCursor variable.

var apiFlags = []flagDef{
	{long: "hostname", value: true},
	{long: "method", short: "X", value: true},
	{long: "input", value: true},
	{long: "raw-field", short: "f", value: true},
	{long: "field", short: "F", value: true},
	{long: "paginate"},
	{long: "header", short: "H", value: true},
}

// maxAPIPages bounds a --paginate walk, so a route whose next-page signal
// never ends fails instead of looping.
const maxAPIPages = 1000

var defaultForgeHost = map[string]string{"gh": "github.com", "glab": "gitlab.com"}

func cliAPI(e *Engine, c *call) response {
	if len(c.positional) != 1 {
		return unhandled("api takes exactly one endpoint, got %d", len(c.positional))
	}
	endpoint := c.positional[0]
	c.http = &httpCall{host: strings.ToLower(defaultString(c.flag("hostname"), defaultForgeHost[c.cli]))}
	c.flags["hostname"] = []string{c.http.host}
	fields, err := apiFields(c)
	if err != nil {
		return unhandled("%v", err)
	}
	for _, header := range c.flags["header"] {
		name, value, ok := strings.Cut(header, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
			return unhandled("header %q is not implemented; only Content-Type is", header)
		}
		c.contentType = strings.TrimSpace(value)
	}
	delete(c.flags, "header")
	if input, ok := c.flags["input"]; ok {
		if len(input) != 1 || input[0] != "-" {
			return unhandled("--input reads only stdin (-) here")
		}
		c.body = c.stdin
	}
	method := strings.ToUpper(c.flag("method"))
	if method == "" {
		method = "GET"
		if c.has("input") || len(fields) > 0 {
			method = "POST"
		}
	}
	c.flags["method"] = []string{method}
	paginate := c.has("paginate")

	if endpoint == "graphql" {
		if c.cli != "gh" {
			return unhandled("glab api graphql is not implemented")
		}
		if method != "POST" || c.has("input") {
			return unhandled("gh api graphql is a POST of -f/-F fields")
		}
		req := graphQLRequest{vars: graphQLVars{}}
		for name, value := range fields {
			switch name {
			case "query":
				req.Query, _ = value.(string)
			case "operationName":
				req.OperationName, _ = value.(string)
			default:
				req.vars[name] = value
			}
		}
		if req.OperationName == "" {
			if m := graphQLSignature.FindStringSubmatch(req.Query); m != nil {
				req.OperationName = m[2]
			}
		}
		if req.Query == "" || req.OperationName == "" {
			return unhandled("gh api graphql needs -f query= with a named operation")
		}
		c.graphQL = &req
		if !paginate {
			return cliRender(c, e.answerAPI(c, true))
		}
		return e.paginateGraphQL(c)
	}
	if len(fields) > 0 {
		return unhandled("REST fields are not implemented; send a body with --input -")
	}
	delete(c.flags, "raw-field")
	delete(c.flags, "field")
	if !paginate {
		return cliRender(c, e.answerAPI(c, true))
	}
	return e.paginateREST(c, endpoint)
}

// apiFields reads -f and -F as a CLI does.
func apiFields(c *call) (map[string]any, error) {
	fields := map[string]any{}
	for _, raw := range c.flags["raw-field"] {
		name, value, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("field %q is not key=value", raw)
		}
		fields[name] = value
	}
	for _, raw := range c.flags["field"] {
		name, value, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("field %q is not key=value", raw)
		}
		switch {
		case value == "true" || value == "false":
			fields[name] = value == "true"
		case value == "null":
			fields[name] = nil
		case value == "@-":
			fields[name] = string(c.stdin)
		case strings.HasPrefix(value, "@"):
			return nil, fmt.Errorf("field %q reads a file; only @- (stdin) is implemented", name)
		default:
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				fields[name] = float64(n)
			} else {
				fields[name] = value
			}
		}
	}
	return fields, nil
}

// paginateREST walks a REST list page by page.
func (e *Engine) paginateREST(c *call, endpoint string) response {
	var pages [][]byte
	resp := response{}
	for range maxAPIPages {
		c.positional = []string{endpoint}
		resp = e.answerAPI(c, true)
		if resp.unhandled != "" || resp.status >= 400 || resp.exit != 0 {
			return cliRender(c, resp)
		}
		if c.cli == "glab" {
			var items []json.RawMessage
			if err := json.Unmarshal(resp.stdout, &items); err != nil {
				return unhandled("--paginate over a non-array answer")
			}
		}
		pages = append(pages, bytes.TrimRight(resp.stdout, "\n"))
		next := nextPage(resp.header)
		if next == "" {
			if c.cli == "glab" {
				resp.stdout = bytes.Join(pages, nil)
			} else {
				resp.stdout = append(bytes.Join(pages, []byte("\n")), '\n')
			}
			return cliRender(c, resp)
		}
		var err error
		if endpoint, err = withPage(endpoint, next); err != nil {
			return unhandled("%v", err)
		}
	}
	return unhandled("--paginate passed %d pages", maxAPIPages)
}

var linkNextPage = regexp.MustCompile(`<([^>]*)>;\s*rel="next"`)

// nextPage is the page number a REST answer names next: GitLab's
// X-Next-Page, or the page parameter of GitHub's Link rel="next".
func nextPage(header http.Header) string {
	if next := header.Get("X-Next-Page"); next != "" {
		return next
	}
	m := linkNextPage.FindStringSubmatch(header.Get("Link"))
	if m == nil {
		return ""
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return ""
	}
	return u.Query().Get("page")
}

func withPage(endpoint, page string) (string, error) {
	path, rawQuery, _ := strings.Cut(endpoint, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", err
	}
	query.Set("page", page)
	return path + "?" + query.Encode(), nil
}

// paginateGraphQL walks a GraphQL connection through $endCursor.
func (e *Engine) paginateGraphQL(c *call) response {
	if !regexp.MustCompile(`\$endCursor\s*:\s*String\b`).MatchString(c.graphQL.Query) {
		return unhandled("--paginate needs the document to declare $endCursor: String")
	}
	var pages [][]byte
	for range maxAPIPages {
		resp := e.answerAPI(c, true)
		if resp.unhandled != "" || resp.status >= 400 || resp.exit != 0 {
			return cliRender(c, resp)
		}
		pages = append(pages, bytes.TrimRight(resp.stdout, "\n"))
		var answer any
		if err := json.Unmarshal(resp.stdout, &answer); err != nil {
			return unhandled("GraphQL answer is not JSON")
		}
		cursor, more := outermostPageInfo(answer)
		if !more {
			resp.stdout = append(bytes.Join(pages, []byte("\n")), '\n')
			return cliRender(c, resp)
		}
		c.graphQL.vars["endCursor"] = cursor
	}
	return unhandled("--paginate passed %d pages", maxAPIPages)
}

// outermostPageInfo finds the shallowest pageInfo and reports its
// endCursor when it has a next page.
func outermostPageInfo(value any) (string, bool) {
	level := []any{value}
	for len(level) > 0 {
		var next []any
		for _, node := range level {
			switch v := node.(type) {
			case map[string]any:
				if info, ok := v["pageInfo"].(map[string]any); ok {
					cursor, _ := info["endCursor"].(string)
					more, _ := info["hasNextPage"].(bool)
					return cursor, more && cursor != ""
				}
				for _, key := range sortedKeys(v) {
					next = append(next, v[key])
				}
			case []any:
				next = append(next, v...)
			}
		}
		level = next
	}
	return "", false
}

// cliRender turns an API answer into what the CLI prints: the body on
// stdout, exit 1 with the forge's message on stderr for an error status,
// and for a GraphQL answer carrying errors.
func cliRender(c *call, resp response) response {
	if resp.unhandled != "" {
		return resp
	}
	if resp.body != nil {
		var buf bytes.Buffer
		_ = resp.body.writeRange(&buf, 0, resp.body.size())
		resp.stdout = buf.Bytes()
		resp.body = nil
	}
	status := resp.status
	if status == 0 {
		status = http.StatusOK
	}
	switch {
	case status >= 400:
		message := strings.TrimSpace(resp.stderr)
		if message == "" {
			message = http.StatusText(status)
		}
		resp.stderr = fmt.Sprintf("%s: %s (HTTP %d)\n", c.cli, message, status)
		resp.exit = 1
	case c.graphQL != nil:
		var answer struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if json.Unmarshal(lastJSONDocument(resp.stdout), &answer) == nil && len(answer.Errors) > 0 {
			messages := make([]string, 0, len(answer.Errors))
			for _, e := range answer.Errors {
				messages = append(messages, e.Message)
			}
			resp.stderr = "gh: " + strings.Join(messages, "\n") + "\n"
			resp.exit = 1
		}
	}
	resp.status, resp.header = 0, nil
	return resp
}

// lastJSONDocument is the last of the newline-joined pages.
func lastJSONDocument(out []byte) []byte {
	out = bytes.TrimRight(out, "\n")
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		return out[i+1:]
	}
	return out
}
