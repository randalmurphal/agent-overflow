package forgefake

import (
	"strings"
	"testing"
)

const queueFixture = `{"viewer": "octo", "repos": [{
	"forge": "github", "project": "acme/merge",
	"merge": {"mergeQueue": true},
	"pulls": [
		{"number": 1, "title": "Ready", "headRef": "ready", "headSha": "1111111111111111111111111111111111111111"},
		{"number": 2, "title": "Checking", "headRef": "checking", "headSha": "2222222222222222222222222222222222222222",
		 "readiness": {"requiredChecks": ["unit"]}, "ci": {"jobs": [{"name": "unit", "status": "running"}]}},
		{"number": 3, "title": "Queued", "headRef": "queued", "headSha": "3333333333333333333333333333333333333333",
		 "queued": {"position": 1, "state": "mergeable", "by": "bob"}}
	]
}]}`

// A merge queue's base: the pulls answer the queue fields, a direct merge
// or a method-bearing auto-merge is refused, enqueue takes a pull that
// could merge now, dequeue takes it out, and a method-less auto-merge
// arms join-when-ready.
func TestGitHubMergeQueueModel(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, queueFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	node := func(number int) string {
		var target struct {
			Data struct {
				Repository struct {
					PullRequest map[string]any `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		decodeBody(t, graphQL(t, srv, "github.com", "PRMergeTarget", map[string]any{"owner": "acme", "name": "merge", "number": number}), &target)
		pull := target.Data.Repository.PullRequest
		if pull["isMergeQueueEnabled"] != true || pull["isInMergeQueue"] != (number == 3) {
			t.Fatalf("PRMergeTarget #%d = %v", number, pull)
		}
		return pull["id"].(string)
	}
	ready, head := node(1), strings.Repeat("1", 40)

	tick := ghTick(t, srv, 1)
	if tick["isMergeQueueEnabled"] != true || tick["isInMergeQueue"] != false || tick["mergeQueueEntry"] != nil || tick["viewerCanEnableAutoMerge"] != true {
		t.Fatalf("an unqueued pull = %v", tick)
	}
	if entry, _ := ghTick(t, srv, 3)["mergeQueueEntry"].(map[string]any); entry == nil || entry["position"] != float64(1) || entry["state"] != "MERGEABLE" ||
		entry["enqueuer"].(map[string]any)["login"] != "bob" || entry["enqueuedAt"] == "" {
		t.Fatalf("a seeded entry = %v", entry)
	}

	if queue, _ := tick["mergeQueue"].(map[string]any); queue == nil || queue["entries"].(map[string]any)["totalCount"] != float64(1) {
		t.Fatalf("the base's queue = %v", tick["mergeQueue"])
	}
	if got := graphQL(t, srv, "github.com", "MergePR", map[string]any{"pullRequestID": ready, "method": "MERGE", "expectedHeadOid": head}); !strings.Contains(got.body, "Changes must be made through the merge queue") {
		t.Fatalf("a direct merge = %s", got.body)
	}
	if got := graphQL(t, srv, "github.com", "EnableAutoMerge", map[string]any{"pullRequestID": ready, "method": "MERGE", "expectedHeadOid": head}); !strings.Contains(got.body, "set by the merge queue") {
		t.Fatalf("an auto-merge with a method = %s", got.body)
	}
	if got := graphQL(t, srv, "github.com", "EnqueuePR", map[string]any{"pullRequestID": ready, "expectedHeadOid": strings.Repeat("f", 40)}); !strings.Contains(got.body, githubHeadModified) {
		t.Fatalf("a stale enqueue = %s", got.body)
	}
	if got := graphQL(t, srv, "github.com", "EnqueuePR", map[string]any{"pullRequestID": ready, "expectedHeadOid": head}); !strings.Contains(got.body, `"mergeQueueEntry":{"id":`) {
		t.Fatalf("enqueue = %s", got.body)
	}
	if entry, _ := ghTick(t, srv, 1)["mergeQueueEntry"].(map[string]any); entry == nil || entry["position"] != float64(2) || entry["state"] != "QUEUED" || entry["enqueuer"].(map[string]any)["login"] != "octo" {
		t.Fatalf("after enqueue = %v", entry)
	}
	if got := graphQL(t, srv, "github.com", "EnqueuePR", map[string]any{"pullRequestID": ready, "expectedHeadOid": head}); !strings.Contains(got.body, "already in the merge queue") {
		t.Fatalf("a second enqueue = %s", got.body)
	}
	if got := graphQL(t, srv, "github.com", "DequeuePR", map[string]any{"pullRequestID": ready}); !strings.Contains(got.body, `"mergeQueueEntry":{"id":`) {
		t.Fatalf("dequeue = %s", got.body)
	}
	if got := ghTick(t, srv, 1); got["isInMergeQueue"] != false || got["mergeQueueEntry"] != nil {
		t.Fatalf("after dequeue = %v", got)
	}
	if got := graphQL(t, srv, "github.com", "DequeuePR", map[string]any{"pullRequestID": ready}); !strings.Contains(got.body, "not in the merge queue") {
		t.Fatalf("a second dequeue = %s", got.body)
	}

	// A pull whose checks run cannot join now; it joins when ready.
	checking, checkingHead := node(2), strings.Repeat("2", 40)
	if got := graphQL(t, srv, "github.com", "EnqueuePR", map[string]any{"pullRequestID": checking, "expectedHeadOid": checkingHead}); !strings.Contains(got.body, "cannot be added to the merge queue") {
		t.Fatalf("enqueue while checks run = %s", got.body)
	}
	if got := graphQL(t, srv, "github.com", "EnableQueuedAutoMerge", map[string]any{"pullRequestID": checking, "expectedHeadOid": checkingHead}); !strings.Contains(got.body, `"enabledAt"`) {
		t.Fatalf("join when ready = %s", got.body)
	}
	if got := ghTick(t, srv, 2); got["autoMergeRequest"] == nil || got["viewerCanEnableAutoMerge"] != false {
		t.Fatalf("after join when ready = %v", got)
	}
	// A pull that could merge now joins at once, as gh relies on.
	if got := graphQL(t, srv, "github.com", "EnableQueuedAutoMerge", map[string]any{"pullRequestID": ready, "expectedHeadOid": head}); !strings.Contains(got.body, `"isInMergeQueue":true`) {
		t.Fatalf("join when ready of a clean pull = %s", got.body)
	}
	if got := ghTick(t, srv, 1); got["isInMergeQueue"] != true || got["autoMergeRequest"] != nil {
		t.Fatalf("after joining when ready = %v", got)
	}
	for _, inv := range e.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}

func TestGitHubMergeQueueSeedRefusals(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"queued without a queue": `{"repos": [{"forge": "github", "project": "a/b", "pulls": [{"number": 1, "title": "t", "queued": {}}]}]}`,
		"queued and closed":      `{"repos": [{"forge": "github", "project": "a/b", "merge": {"mergeQueue": true}, "pulls": [{"number": 1, "title": "t", "state": "closed", "queued": {}}]}]}`,
		"an unknown state":       `{"repos": [{"forge": "github", "project": "a/b", "merge": {"mergeQueue": true}, "pulls": [{"number": 1, "title": "t", "queued": {"state": "waiting"}}]}]}`,
		"a GitLab queue":         `{"repos": [{"forge": "gitlab", "project": "a/b", "merge": {"mergeQueue": true}}]}`,
	} {
		if _, err := seedJSON(New(Options{APIToken: testAPIToken}), raw); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
}
