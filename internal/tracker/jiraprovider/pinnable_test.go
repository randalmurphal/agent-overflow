package jiraprovider

import (
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
)

// The fields a project can pin are the catalog's fields a detail lists:
// none of the card's, the description, the sprint or the rank, each once,
// by name, a textarea a section; one catalog read serves a second call.
func TestFieldsAreTheCatalogsFieldsADetailLists(t *testing.T) {
	f, p := newSite(t)
	for range 2 {
		got, err := p.Fields(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, field := range got {
			listed = append(listed, string(field.ID)+":"+field.Kind.String())
		}
		want := "components:option,duedate:date,customfield_10040:rich_text,timeoriginalestimate:number,customfield_10070:read_only," +
			"reporter:user,resolution:option,customfield_10060:user,customfield_10050:option,customfield_10016:number"
		if strings.Join(listed, ",") != want {
			t.Fatalf("fields\n%s\nwant\n%s", strings.Join(listed, ","), want)
		}
		for _, field := range got {
			if field.ID == tracker.FieldID("customfield_10060") && !field.List {
				t.Errorf("Reviewers is not a list: %+v", field)
			}
		}
	}
	if n := f.Count("GET", fieldPath); n != 1 {
		t.Fatalf("%d catalog reads, want 1", n)
	}
}
