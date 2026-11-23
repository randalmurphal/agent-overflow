package tempo

import (
	"encoding/json"
	"fmt"
	"strings"
)

// wl is an invented worklog in Tempo's wire shape.
type wl struct {
	id       int64
	issue    int64
	author   string
	date     string
	start    string
	spent    int64
	billable *int64
	desc     string
	attrs    [][2]string
}

func (w wl) json() string {
	values := make([]map[string]string, len(w.attrs))
	for i, a := range w.attrs {
		values[i] = map[string]string{"key": a[0], "value": a[1]}
	}
	m := map[string]any{
		"self":             fmt.Sprintf("https://api.example.com/4/worklogs/%d", w.id),
		"tempoWorklogId":   w.id,
		"issue":            map[string]any{"id": w.issue, "self": "https://example.atlassian.net/rest/api/2/issue/1"},
		"author":           map[string]any{"accountId": w.author, "self": "https://example.atlassian.net/rest/api/2/user"},
		"startDate":        w.date,
		"timeSpentSeconds": w.spent,
		"description":      w.desc,
		"attributes":       map[string]any{"self": "https://api.example.com/4/worklogs/1/work-attribute-values", "values": values},
		"createdAt":        "2026-11-02T08:00:00Z",
		"updatedAt":        "2026-11-03T09:30:00Z",
	}
	if w.start != "" {
		m["startTime"] = w.start
		m["startDateTimeUtc"] = w.date + "T" + w.start + "Z"
	}
	if w.billable != nil {
		m["billableSeconds"] = *w.billable
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// pageOf is Tempo's list envelope around items; next names a following
// page when more is set.
func pageOf(offset int, more bool, items ...string) string {
	next := ""
	if more {
		next = fmt.Sprintf(`,"next":"https://api.example.com/4/next?offset=%d"`, offset+len(items))
	}
	return fmt.Sprintf(`{"self":"https://api.example.com/4/x","metadata":{"count":%d,"offset":%d,"limit":1000%s},"results":[%s]}`,
		len(items), offset, next, strings.Join(items, ","))
}

// attributesJSON is a site's invented work attributes: a required static
// list, a checkbox, a number, an account and a text field.
const attributesJSON = `{"self":"https://api.example.com/4/work-attributes","metadata":{"count":5,"offset":0,"limit":1000},"results":[
 {"self":"x","key":"_Category_","name":"Category","type":"STATIC_LIST","required":true,
  "values":["dev","mtg"],"names":{"dev":"Development","mtg":"Meeting"}},
 {"self":"x","key":"_Overtime_","name":"Overtime","type":"CHECKBOX","required":false},
 {"self":"x","key":"_Units_","name":"Units","type":"INPUT_NUMERIC","required":false},
 {"self":"x","key":"_Account_","name":"Account","type":"ACCOUNT","required":false},
 {"self":"x","key":"_Note_","name":"Note","type":"INPUT_FIELD","required":false}]}`

// approvalJSONFor is an approval of the week from..to in status.
func approvalJSONFor(from, to, status string) string {
	return fmt.Sprintf(`{"self":"x","actions":{},"worklogs":{"self":"x"},"user":{"accountId":"acc-user1","self":"x"},
"period":{"from":%q,"to":%q},"requiredSeconds":144000,"timeSpentSeconds":72000,
"reviewer":{"accountId":"acc-user2","self":"x"},
"status":{"key":%q,"actor":{"accountId":"acc-user1","self":"x"},"updatedAt":"2026-11-08T10:00:00Z"}}`, from, to, status)
}

func seconds64(n int64) *int64 { return &n }
