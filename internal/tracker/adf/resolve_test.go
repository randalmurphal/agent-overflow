package adf

import (
	"strings"
	"testing"
)

// mentionDoc is a doc of one paragraph mentioning each id as @name.
func mentionDoc(name string, ids ...string) *Node {
	p := &Node{Type: "paragraph"}
	for i, id := range ids {
		if i > 0 {
			p.Content = append(p.Content, &Node{Type: "text", Text: " and "})
		}
		p.Content = append(p.Content, &Node{Type: "mention",
			Attrs: map[string]any{"id": id, "text": "@" + name}})
	}
	return &Node{Type: "doc", Attrs: map[string]any{"version": 1}, Content: []*Node{p}}
}

// TestSharedMentionNameAsksResolver proves a display name the original
// mentions for two accounts is not taken from it: the Resolver decides,
// and its refusal stands, where before the last account seen won.
func TestSharedMentionNameAsksResolver(t *testing.T) {
	orig := mentionDoc("User 1", "acc-old", "acc-other")
	r := newFakeResolver()
	doc, _, err := Edit(t.Context(), orig, "Ask @[User 1] first", r)
	if err != nil || r.calls != 1 {
		t.Fatalf("Edit: %v (calls %d)", err, r.calls)
	}
	if js := mustJSON(doc); !strings.Contains(js, `"id":"acc-user1"`) ||
		strings.Contains(js, "acc-old") || strings.Contains(js, "acc-other") {
		t.Errorf("mention not from the resolver: %s", js)
	}
	r.people = map[string][2]string{}
	if _, _, err := Edit(t.Context(), orig, "Ask @[User 1] first", r); err == nil ||
		!strings.Contains(err.Error(), `no user matches "User 1"`) {
		t.Errorf("resolver refusal lost: %v", err)
	}
}

// TestUniqueMentionNameReusesOrig proves a name the original mentions for
// one account (however often) still resolves from it without a lookup.
func TestUniqueMentionNameReusesOrig(t *testing.T) {
	orig := mentionDoc("User 1", "acc-old", "acc-old")
	r := newFakeResolver()
	doc, _, err := Edit(t.Context(), orig, "Ask @[User 1] first", r)
	if err != nil || r.calls != 0 || !strings.Contains(mustJSON(doc), `"id":"acc-old"`) {
		t.Errorf("Edit: %v (calls %d) %s", err, r.calls, mustJSON(doc))
	}
}
