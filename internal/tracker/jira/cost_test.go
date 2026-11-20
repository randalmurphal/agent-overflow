package jira

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"testing"
)

// The cost benchmarks measure the client's hot reads at realistic sizes:
// a 100-issue page of board rows as the delta reads it, the same page
// through a plain encoding/json/v2 struct for comparison, and a cached
// Fields call on a 1,500-field catalog with a lookup by ID.

// costRowPage is a page of n issues with the fields a board row reads:
// the card's system fields, the sprint field, the rank and a story point
// estimate, in Jira Cloud's shapes (self links, avatar URLs, status
// categories).
func costRowPage(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"isLast":false,"nextPageToken":"token-1","issues":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%d","key":"WEB-%d","self":"https://example.atlassian.net/rest/api/3/issue/%d","fields":{`, 10000+i, i+1, 10000+i)
		fmt.Fprintf(&b, `"summary":"Issue %d with a title of ordinary length",`, i+1)
		b.WriteString(`"status":{"self":"https://example.atlassian.net/rest/api/3/status/10001","description":"","iconUrl":"https://example.atlassian.net/images/icons/statuses/generic.png","name":"In Progress","id":"10001","statusCategory":{"self":"https://example.atlassian.net/rest/api/3/statuscategory/4","id":4,"key":"indeterminate","colorName":"yellow","name":"In Progress"}},`)
		b.WriteString(`"issuetype":{"self":"https://example.atlassian.net/rest/api/3/issuetype/10002","id":"10002","description":"A task.","iconUrl":"https://example.atlassian.net/images/icons/task.svg","name":"Task","subtask":false,"hierarchyLevel":0},`)
		b.WriteString(`"assignee":{"self":"https://example.atlassian.net/rest/api/3/user?accountId=acc-user1","accountId":"acc-user1","avatarUrls":{"48x48":"https://avatar.example.com/48.png","24x24":"https://avatar.example.com/24.png","16x16":"https://avatar.example.com/16.png","32x32":"https://avatar.example.com/32.png"},"displayName":"User 1","active":true,"timeZone":"UTC","accountType":"atlassian"},`)
		b.WriteString(`"priority":{"self":"https://example.atlassian.net/rest/api/3/priority/3","iconUrl":"https://example.atlassian.net/images/icons/priorities/medium.svg","name":"Medium","id":"3"},`)
		b.WriteString(`"project":{"self":"https://example.atlassian.net/rest/api/3/project/10000","id":"10000","key":"WEB","name":"Web","projectTypeKey":"software","simplified":false},`)
		b.WriteString(`"labels":["frontend"],"parent":null,"created":"2026-11-01T09:00:00.000+0000","updated":"2026-11-18T09:00:00.000+0000","resolutiondate":null,`)
		b.WriteString(`"customfield_10020":[{"id":1,"name":"Sprint 1","state":"active","boardId":1,"goal":"","startDate":"2026-11-10T09:00:00.000Z","endDate":"2026-11-24T09:00:00.000Z"}],`)
		fmt.Fprintf(&b, `"customfield_10019":"0|i%05d:","customfield_10016":3}}`, i)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// searchAnswer is the answer shape Search decodes.
type searchAnswer struct {
	Issues        []issueJSON `json:"issues"`
	NextPageToken string      `json:"nextPageToken"`
	IsLast        bool        `json:"isLast"`
}

// BenchmarkDecodeRowPage is a 100-issue row page decoded as Search and
// ListIssues decode it: each issue's fields in one pass (issueFields),
// typed fields converted to an Issue and the others copied raw.
func BenchmarkDecodeRowPage(b *testing.B) {
	body := costRowPage(100)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		var v searchAnswer
		if err := json.Unmarshal(body, &v); err != nil {
			b.Fatal(err)
		}
		issues := make([]*Issue, len(v.Issues))
		for i, is := range v.Issues {
			issues[i] = is.decode()
		}
		if len(issues) != 100 || issues[99].Status == nil {
			b.Fatal("decode")
		}
	}
}

// onePassFields is the issue fields decoded by a plain encoding/json/v2
// struct: the system fields a card reads typed, every other field kept
// raw through the embedded fallback member, with no conversion and no
// record of a field that fails to decode.
type onePassFields struct {
	Summary   string                    `json:"summary"`
	Status    *statusJSON               `json:"status"`
	IssueType *issueTypeJSON            `json:"issuetype"`
	Priority  *priorityJSON             `json:"priority"`
	Assignee  *userJSON                 `json:"assignee"`
	Project   *projectJSON              `json:"project"`
	Parent    *issueRefJSON             `json:"parent"`
	Labels    []string                  `json:"labels"`
	Created   string                    `json:"created"`
	Updated   string                    `json:"updated"`
	Rest      map[string]jsontext.Value `json:",embed"`
}

type onePassAnswer struct {
	Issues []struct {
		ID     string        `json:"id"`
		Key    string        `json:"key"`
		Fields onePassFields `json:"fields"`
	} `json:"issues"`
	NextPageToken string `json:"nextPageToken"`
	IsLast        bool   `json:"isLast"`
}

// BenchmarkDecodeRowPageOnePass is the same page through onePassFields,
// the floor BenchmarkDecodeRowPage is compared with.
func BenchmarkDecodeRowPageOnePass(b *testing.B) {
	body := costRowPage(100)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		var v onePassAnswer
		if err := jsonv2.Unmarshal(body, &v, jsonv2.MatchCaseInsensitiveNames(true)); err != nil {
			b.Fatal(err)
		}
		if len(v.Issues) != 100 || v.Issues[99].Fields.Status == nil || len(v.Issues[99].Fields.Rest) != 4 {
			b.Fatalf("decode: %d rest", len(v.Issues[99].Fields.Rest))
		}
	}
}

// costCatalog is a site's field catalog of n fields.
func costCatalog(n int) []Field {
	out := make([]Field, n)
	for i := range out {
		id := fmt.Sprintf("customfield_%d", 10000+i)
		out[i] = Field{ID: id, Name: fmt.Sprintf("Field %d", i), Custom: true,
			Schema: Schema{Type: "string", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:textfield", CustomID: int64(10000 + i)}}
	}
	return out
}

// BenchmarkCatalogGet is one cached Fields call on a site of 1,500
// fields, and a lookup by ID in what it hands out, as every provider read
// that needs the sprint field makes.
func BenchmarkCatalogGet(b *testing.B) {
	c := newCache[struct{}, *Catalog](1, nil)
	c.put(c.epoch(), struct{}{}, newCatalog(costCatalog(1500)))
	b.ReportAllocs()
	for b.Loop() {
		cat, ok := c.get(struct{}{})
		if !ok || cat.Len() != 1500 {
			b.Fatal("miss")
		}
		if _, ok := cat.Field("customfield_11499"); !ok {
			b.Fatal("lookup")
		}
	}
}
