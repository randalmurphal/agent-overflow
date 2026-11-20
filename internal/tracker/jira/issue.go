package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/url"
	"strings"
	"time"

	"agent-overflow/internal/tracker/adf"
)

// Issue is an issue as read: typed values for the fields every site has,
// and every other field's raw value by field ID. A field missing from
// the answer (not requested, or not visible) is zero. A typed field whose
// value had an unexpected shape stays zero, keeps its value in Raw and is
// named in Problems; the rest of the issue still decodes.
type Issue struct {
	ID  string
	Key string

	Summary     string
	Status      *Status
	Type        *IssueType
	Priority    *Priority
	Assignee    *User
	Reporter    *User
	Parent      *IssueRef
	Labels      []string
	Components  []Component
	FixVersions []Version
	Created     time.Time
	Updated     time.Time
	DueDate     Date
	Resolution  *Resolution
	Project     *Project
	// Description is the description's ADF document; adf.ToMarkdown
	// renders it.
	Description *adf.Node
	Attachments []Attachment
	Links       []IssueLink
	Subtasks    []IssueRef

	// Raw holds every field not decoded above, by field ID, as Jira sent
	// it; Field reads one with a typed reader.
	Raw map[string]json.RawMessage
	// Problems names the typed fields whose value did not decode.
	Problems []FieldProblem
}

// FieldProblem is a field whose value did not have the shape its type
// promised. The value is in Issue.Raw.
type FieldProblem struct {
	Field string
	Err   error
}

// IssueRef is another issue as an issue names it: a parent, a subtask, a
// linked issue.
type IssueRef struct {
	ID       string
	Key      string
	Summary  string
	Status   *Status
	Type     *IssueType
	Priority *Priority
}

// Component is a project component.
type Component struct {
	ID   string
	Name string
}

// Version is a project version.
type Version struct {
	ID          string
	Name        string
	Released    bool
	Archived    bool
	ReleaseDate Date
}

// Resolution is how an issue was resolved.
type Resolution struct {
	ID   string
	Name string
}

// LinkDirection says how the other issue of a link relates to this one.
type LinkDirection string

const (
	// Outward: this issue relates to the other by the type's Outward
	// phrase ("blocks").
	Outward LinkDirection = "outward"
	// Inward: by the Inward phrase ("is blocked by").
	Inward LinkDirection = "inward"
)

// IssueLink is a link from this issue to Issue.
type IssueLink struct {
	ID        string
	Type      LinkType
	Direction LinkDirection
	Issue     IssueRef
}

// Field returns the raw value of a field not decoded into Issue's typed
// fields, with typed readers. A field the answer lacks reads as null.
func (is *Issue) Field(id string) FieldValue { return FieldValue(is.Raw[id]) }

// issueJSON is an issue as the API sends it.
type issueJSON struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Fields issueFields `json:"fields"`
}

// fieldDecoder decodes one typed field's value, which is not null, into
// is, with the options of the answer it is part of.
type fieldDecoder func(is *Issue, raw jsontext.Value, opts jsonv2.Options) error

// systemFields decode the typed fields; one entry per field ID.
var systemFields = map[string]fieldDecoder{
	"summary": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return jsonv2.Unmarshal(raw, &is.Summary, o)
	},
	"labels": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return jsonv2.Unmarshal(raw, &is.Labels, o)
	},
	"status": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Status, statusJSON.status)
	},
	"issuetype": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Type, issueTypeJSON.issueType)
	},
	"priority": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Priority, priorityOf)
	},
	"assignee": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Assignee, userJSON.user)
	},
	"reporter": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Reporter, userJSON.user)
	},
	"parent": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Parent, issueRefJSON.ref)
	},
	"resolution": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Resolution, resolutionOf)
	},
	"project": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeInto(raw, o, &is.Project, projectJSON.project)
	},
	"description": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeDocWith(raw, o, &is.Description)
	},
	"created": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error { return decodeTime(raw, o, &is.Created) },
	"updated": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error { return decodeTime(raw, o, &is.Updated) },
	"duedate": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return jsonv2.Unmarshal(raw, &is.DueDate, o)
	},
	"components": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeList(raw, o, &is.Components, componentOf)
	},
	"fixVersions": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeList(raw, o, &is.FixVersions, versionJSON.version)
	},
	"attachment": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeList(raw, o, &is.Attachments, attachmentJSON.attachment)
	},
	"subtasks": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeList(raw, o, &is.Subtasks, issueRefJSON.ref)
	},
	"issuelinks": func(is *Issue, raw jsontext.Value, o jsonv2.Options) error {
		return decodeList(raw, o, &is.Links, issueLinkJSON.link)
	},
}

// issueFields is an issue's fields object, read in one pass over the
// answer: each typed field decodes from its value where it lies in the
// answer, and every other field is kept raw. A typed field whose value
// does not decode stays zero, keeps its value in Raw and is named in
// Problems.
type issueFields struct {
	is Issue
}

// UnmarshalJSONFrom reads the fields object, or null, from dec.
func (f *issueFields) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	opts := dec.Options()
	if dec.PeekKind() == 'n' {
		_, err := dec.ReadToken()
		return err
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("jira: an issue's fields are a JSON %s, not an object", tok.Kind())
	}
	is := &f.is
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return err
		}
		id := name.String()
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		decode, typed := systemFields[id]
		switch {
		case !typed:
			is.keep(id, raw)
		case nullValue(raw):
		default:
			if err := decode(is, raw, opts); err != nil {
				is.keep(id, raw)
				is.Problems = append(is.Problems, FieldProblem{Field: id, Err: err})
			}
		}
	}
	_, err = dec.ReadToken()
	return err
}

// keep stores a copy of raw, which is only valid until the decoder reads
// on, as the raw value of field id.
func (is *Issue) keep(id string, raw jsontext.Value) {
	if is.Raw == nil {
		is.Raw = map[string]json.RawMessage{}
	}
	is.Raw[id] = json.RawMessage(bytes.Clone(raw))
}

// nullValue reports a JSON null; a value from the decoder carries no
// surrounding space.
func nullValue(raw jsontext.Value) bool { return string(raw) == "null" }

// decode is v as an Issue.
func (v issueJSON) decode() *Issue {
	is := v.Fields.is
	is.ID, is.Key = v.ID, v.Key
	return &is
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// decodeInto decodes raw as J and stores conv of it in *dst. A failure
// leaves *dst nil.
func decodeInto[J, T any](raw jsontext.Value, opts jsonv2.Options, dst **T, conv func(J) T) error {
	var j J
	if err := jsonv2.Unmarshal(raw, &j, opts); err != nil {
		return err
	}
	v := conv(j)
	*dst = &v
	return nil
}

// decodeList decodes raw as a list of J and stores conv of each in *dst.
func decodeList[J, T any](raw jsontext.Value, opts jsonv2.Options, dst *[]T, conv func(J) T) error {
	var js []J
	if err := jsonv2.Unmarshal(raw, &js, opts); err != nil {
		return err
	}
	out := make([]T, len(js))
	for i, j := range js {
		out[i] = conv(j)
	}
	*dst = out
	return nil
}

// decodeDoc decodes raw, read on its own, as an ADF document.
func decodeDoc(raw json.RawMessage, dst **adf.Node) error {
	return docOf(raw, nil, dst)
}

// decodeDocWith decodes raw, a value of an answer read with opts, as an
// ADF document.
func decodeDocWith(raw jsontext.Value, opts jsonv2.Options, dst **adf.Node) error {
	return docOf(json.RawMessage(raw), opts, dst)
}

func docOf(raw json.RawMessage, opts jsonv2.Options, dst **adf.Node) error {
	var doc adf.Node
	var err error
	if opts == nil {
		err = json.Unmarshal(raw, &doc)
	} else {
		err = jsonv2.Unmarshal(raw, &doc, opts)
	}
	if err != nil {
		return err
	}
	if doc.Type != "doc" {
		return fmt.Errorf("rich text of type %q, not an ADF doc", doc.Type)
	}
	*dst = &doc
	return nil
}

func decodeTime(raw jsontext.Value, opts jsonv2.Options, dst *time.Time) error {
	var s string
	if err := jsonv2.Unmarshal(raw, &s, opts); err != nil {
		return err
	}
	t, err := parseTime(s)
	if err != nil {
		return err
	}
	*dst = t
	return nil
}

func priorityOf(p priorityJSON) Priority { return Priority(p) }

type namedJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func resolutionOf(r namedJSON) Resolution { return Resolution(r) }
func componentOf(r namedJSON) Component   { return Component(r) }

type versionJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Released    bool   `json:"released"`
	Archived    bool   `json:"archived"`
	ReleaseDate Date   `json:"releaseDate"`
}

func (v versionJSON) version() Version { return Version(v) }

// issueRefJSON is another issue as an issue's fields name it.
type issueRefJSON struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary   string         `json:"summary"`
		Status    *statusJSON    `json:"status"`
		IssueType *issueTypeJSON `json:"issuetype"`
		Priority  *priorityJSON  `json:"priority"`
	} `json:"fields"`
}

func (r issueRefJSON) ref() IssueRef {
	out := IssueRef{ID: r.ID, Key: r.Key, Summary: r.Fields.Summary}
	if s := r.Fields.Status; s != nil {
		v := s.status()
		out.Status = &v
	}
	if t := r.Fields.IssueType; t != nil {
		v := t.issueType()
		out.Type = &v
	}
	if p := r.Fields.Priority; p != nil {
		v := Priority(*p)
		out.Priority = &v
	}
	return out
}

type issueLinkJSON struct {
	ID   string `json:"id"`
	Type struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Inward  string `json:"inward"`
		Outward string `json:"outward"`
	} `json:"type"`
	InwardIssue  *issueRefJSON `json:"inwardIssue"`
	OutwardIssue *issueRefJSON `json:"outwardIssue"`
}

func (l issueLinkJSON) link() IssueLink {
	out := IssueLink{ID: l.ID, Type: LinkType(l.Type)}
	switch {
	case l.OutwardIssue != nil:
		out.Direction, out.Issue = Outward, l.OutwardIssue.ref()
	case l.InwardIssue != nil:
		out.Direction, out.Issue = Inward, l.InwardIssue.ref()
	}
	return out
}

// GetIssue reads one issue with the fields named (IDs, or *all,
// *navigable and -ID exclusions); none named reads *all.
func (c *Client) GetIssue(ctx context.Context, ref string, fields ...string) (*Issue, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	path, err := issuePath(ref)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		fields = []string{"*all"}
	}
	var v issueJSON
	if err := c.get(ctx, path, url.Values{"fields": {strings.Join(fields, ",")}}, &v); err != nil {
		return nil, fmt.Errorf("jira: get issue %s: %w", ref, err)
	}
	return v.decode(), nil
}
