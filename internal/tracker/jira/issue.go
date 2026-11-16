package jira

import (
	"context"
	"encoding/json"
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
	ID     string                     `json:"id"`
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// systemFields decode the typed fields; one entry per field ID. Each
// reads a value that is not null.
var systemFields = map[string]func(is *Issue, raw json.RawMessage) error{
	"summary":     func(is *Issue, raw json.RawMessage) error { return json.Unmarshal(raw, &is.Summary) },
	"labels":      func(is *Issue, raw json.RawMessage) error { return json.Unmarshal(raw, &is.Labels) },
	"status":      func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Status, statusJSON.status) },
	"issuetype":   func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Type, issueTypeJSON.issueType) },
	"priority":    func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Priority, priorityOf) },
	"assignee":    func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Assignee, userJSON.user) },
	"reporter":    func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Reporter, userJSON.user) },
	"parent":      func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Parent, issueRefJSON.ref) },
	"resolution":  func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Resolution, resolutionOf) },
	"project":     func(is *Issue, raw json.RawMessage) error { return decodeInto(raw, &is.Project, projectJSON.project) },
	"description": func(is *Issue, raw json.RawMessage) error { return decodeDoc(raw, &is.Description) },
	"created":     func(is *Issue, raw json.RawMessage) error { return decodeTime(raw, &is.Created) },
	"updated":     func(is *Issue, raw json.RawMessage) error { return decodeTime(raw, &is.Updated) },
	"duedate":     func(is *Issue, raw json.RawMessage) error { return json.Unmarshal(raw, &is.DueDate) },
	"components":  func(is *Issue, raw json.RawMessage) error { return decodeList(raw, &is.Components, componentOf) },
	"fixVersions": func(is *Issue, raw json.RawMessage) error {
		return decodeList(raw, &is.FixVersions, versionJSON.version)
	},
	"attachment": func(is *Issue, raw json.RawMessage) error {
		return decodeList(raw, &is.Attachments, attachmentJSON.attachment)
	},
	"subtasks":   func(is *Issue, raw json.RawMessage) error { return decodeList(raw, &is.Subtasks, issueRefJSON.ref) },
	"issuelinks": func(is *Issue, raw json.RawMessage) error { return decodeList(raw, &is.Links, issueLinkJSON.link) },
}

// decode turns v into an Issue, field by field.
func (v issueJSON) decode() *Issue {
	is := &Issue{ID: v.ID, Key: v.Key}
	for id, raw := range v.Fields {
		dec, typed := systemFields[id]
		if !typed {
			if is.Raw == nil {
				is.Raw = map[string]json.RawMessage{}
			}
			is.Raw[id] = raw
			continue
		}
		if isNull(raw) {
			continue
		}
		if err := dec(is, raw); err != nil {
			if is.Raw == nil {
				is.Raw = map[string]json.RawMessage{}
			}
			is.Raw[id] = raw
			is.Problems = append(is.Problems, FieldProblem{Field: id, Err: err})
		}
	}
	return is
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// decodeInto decodes raw as J and stores conv of it in *dst. A failure
// leaves *dst nil.
func decodeInto[J, T any](raw json.RawMessage, dst **T, conv func(J) T) error {
	var j J
	if err := json.Unmarshal(raw, &j); err != nil {
		return err
	}
	v := conv(j)
	*dst = &v
	return nil
}

// decodeList decodes raw as a list of J and stores conv of each in *dst.
func decodeList[J, T any](raw json.RawMessage, dst *[]T, conv func(J) T) error {
	var js []J
	if err := json.Unmarshal(raw, &js); err != nil {
		return err
	}
	out := make([]T, len(js))
	for i, j := range js {
		out[i] = conv(j)
	}
	*dst = out
	return nil
}

func decodeDoc(raw json.RawMessage, dst **adf.Node) error {
	var doc adf.Node
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.Type != "doc" {
		return fmt.Errorf("rich text of type %q, not an ADF doc", doc.Type)
	}
	*dst = &doc
	return nil
}

func decodeTime(raw json.RawMessage, dst *time.Time) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
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
