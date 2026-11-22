package jira

import "testing"

// Every kind has its rule, at its index.
func TestEveryKindHasARule(t *testing.T) {
	t.Parallel()
	if len(rules) != int(kindTeam)+1 {
		t.Fatalf("%d rules for %d kinds", len(rules), kindTeam+1)
	}
	for k, r := range rules {
		if r.what == "" || (k != int(kindUnknown)) != (r.build != nil) {
			t.Errorf("kind %d: rule %+v", k, r)
		}
	}
}
