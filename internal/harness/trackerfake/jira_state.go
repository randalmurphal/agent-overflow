package trackerfake

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// statusCategories are Jira's status categories by key: ID, name and
// color, as the platform API names them.
var statusCategories = map[string]struct {
	id          int
	name, color string
}{
	"new":           {2, "To Do", "blue-gray"},
	"indeterminate": {4, "In Progress", "yellow"},
	"done":          {3, "Done", "green"},
}

// The custom field types with behavior of their own.
const (
	sprintFieldType = "com.pyxis.greenhopper.jira:gh-sprint"
	rankFieldType   = "com.pyxis.greenhopper.jira:gh-lexo-rank"
)

func (s *jiraSite) user(id string) *JiraUser {
	for i := range s.Users {
		if s.Users[i].AccountID == id {
			return &s.Users[i]
		}
	}
	return nil
}

func (s *jiraSite) status(id string) *JiraStatus {
	for i := range s.Statuses {
		if s.Statuses[i].ID == id {
			return &s.Statuses[i]
		}
	}
	return nil
}

func (s *jiraSite) priority(id string) *JiraNamed {
	for i := range s.Priorities {
		if s.Priorities[i].ID == id {
			return &s.Priorities[i]
		}
	}
	return nil
}

// project finds a project by key (case ignored) or ID.
func (s *jiraSite) project(ref string) *JiraProject {
	for i := range s.Projects {
		if p := &s.Projects[i]; p.ID == ref || strings.EqualFold(p.Key, ref) {
			return p
		}
	}
	return nil
}

func (p *JiraProject) issueType(id string) *JiraIssueType {
	for i := range p.IssueTypes {
		if p.IssueTypes[i].ID == id {
			return &p.IssueTypes[i]
		}
	}
	return nil
}

func (s *jiraSite) board(id int64) *JiraBoard {
	for i := range s.Boards {
		if s.Boards[i].ID == id {
			return &s.Boards[i]
		}
	}
	return nil
}

func (s *jiraSite) sprint(id int64) *JiraSprint {
	for i := range s.Sprints {
		if s.Sprints[i].ID == id {
			return &s.Sprints[i]
		}
	}
	return nil
}

// issue finds an issue by numeric ID or key, the key's case ignored.
func (s *jiraSite) issue(ref string) *JiraIssue {
	for i := range s.Issues {
		if is := &s.Issues[i]; is.ID == ref || strings.EqualFold(is.Key, ref) {
			return is
		}
	}
	return nil
}

// linkType finds a link type by ID or name.
func (s *jiraSite) linkType(ref string) *JiraLinkType {
	for i := range s.LinkTypes {
		if t := &s.LinkTypes[i]; t.ID == ref || t.Name == ref {
			return t
		}
	}
	return nil
}

// customField finds a custom field by ID.
func (s *jiraSite) customField(id string) *JiraField {
	for i := range s.Fields {
		if s.Fields[i].ID == id {
			return &s.Fields[i]
		}
	}
	return nil
}

// fieldOfType is the site's custom field of a type with behavior.
func (s *jiraSite) fieldOfType(custom string) *JiraField {
	for i := range s.Fields {
		if s.Fields[i].Schema.Custom == custom {
			return &s.Fields[i]
		}
	}
	return nil
}

func (is *JiraIssue) comment(id string) *JiraComment {
	for i := range is.Comments {
		if is.Comments[i].ID == id {
			return &is.Comments[i]
		}
	}
	return nil
}

func (is *JiraIssue) worklog(id string) *JiraWorklog {
	for i := range is.Worklogs {
		if is.Worklogs[i].ID == id {
			return &is.Worklogs[i]
		}
	}
	return nil
}

// attachment finds an attachment of any issue, and its issue.
func (s *jiraSite) attachment(id string) (*JiraIssue, int) {
	for i := range s.Issues {
		for j := range s.Issues[i].Attachments {
			if s.Issues[i].Attachments[j].ID == id {
				return &s.Issues[i], j
			}
		}
	}
	return nil, -1
}

// boardIssues are the issues board b lists, in rank order: those of its
// project whose status a column maps.
func (s *jiraSite) boardIssues(b *JiraBoard) []*JiraIssue {
	var mapped []string
	for _, col := range b.Columns {
		mapped = append(mapped, col.Statuses...)
	}
	var out []*JiraIssue
	p := s.project(b.Project)
	for i := range s.Issues {
		is := &s.Issues[i]
		if is.Project == p.Key && slices.Contains(mapped, is.Status) {
			out = append(out, is)
		}
	}
	sortByRank(out)
	return out
}

// doneStatuses are the statuses of a board's last column that maps any.
func (b *JiraBoard) doneStatuses() []string {
	for i := len(b.Columns) - 1; i >= 0; i-- {
		if len(b.Columns[i].Statuses) > 0 {
			return b.Columns[i].Statuses
		}
	}
	return nil
}

// openSprint reports a sprint an issue sits in: future or active.
func (s *jiraSite) openSprint(id int64) bool {
	sp := s.sprint(id)
	return sp != nil && sp.State != "closed"
}

func sortByRank(issues []*JiraIssue) {
	slices.SortStableFunc(issues, func(a, b *JiraIssue) int { return strings.Compare(a.Rank, b.Rank) })
}

// rankDigits is the alphabet of a rank's position; byte order is rank
// order.
const rankDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

// rankPrefix opens every rank value, as Jira's LexoRank values open with
// their bucket.
const rankPrefix = "0|"

// lastRank is a rank after every issue's.
func (s *jiraSite) lastRank() string {
	top := ""
	for _, is := range s.Issues {
		top = max(top, strings.TrimPrefix(is.Rank, rankPrefix))
	}
	return rankPrefix + midRank(top, "")
}

// midRank is a position strictly between a and b, b empty meaning after
// every position. Positions use rankDigits and never end in its first
// digit, so one always lies between two.
func midRank(a, b string) string {
	if b != "" {
		n := 0
		for n < len(b) && rankDigit(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + midRank(a[min(n, len(a)):], b[n:])
		}
	}
	lo, hi := 0, len(rankDigits)
	if a != "" {
		lo = strings.IndexByte(rankDigits, a[0])
	}
	if b != "" {
		hi = strings.IndexByte(rankDigits, b[0])
	}
	if hi-lo > 1 {
		return string(rankDigits[(lo+hi)/2])
	}
	if len(b) > 1 {
		return b[:1]
	}
	rest := ""
	if a != "" {
		rest = a[1:]
	}
	return string(rankDigits[lo]) + midRank(rest, "")
}

func rankDigit(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return rankDigits[0]
}

// mediaID is a media file ID in the canonical UUID form Jira's media
// service uses, made from n.
func mediaID(n int64) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", n>>48, n&0xffffffffffff)
}

// validDoc reports an ADF document: an object of type doc.
func validDoc(raw json.RawMessage) bool {
	var doc struct {
		Type    string          `json:"type"`
		Version int             `json:"version"`
		Content json.RawMessage `json:"content"`
	}
	return json.Unmarshal(raw, &doc) == nil && doc.Type == "doc"
}

// docText is the plain text of an ADF document, its text nodes joined
// by spaces, for the text search.
func docText(raw json.RawMessage) string {
	var node struct {
		Text    string            `json:"text"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return ""
	}
	parts := []string{node.Text}
	for _, c := range node.Content {
		parts = append(parts, docText(c))
	}
	return strings.Join(parts, " ")
}

// idNumber is a numeric ID's value; zero for anything else.
func idNumber(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}
