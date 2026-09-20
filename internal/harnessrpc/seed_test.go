package harnessrpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness"
)

func TestHarnessSeedRefusesTraversalProjectNames(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	for _, name := range []string{"../outside", "a/b", `a\b`, ".", ".."} {
		_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
			Name: name,
			Repo: &harness.RepoSpec{},
		}}})
		if err == nil || !strings.Contains(err.Error(), "plain directory name") {
			t.Fatalf("HarnessSeed(name=%q): err = %v, want plain-directory-name refusal", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the workspaces root (stat err %v)", err)
	}
}

func TestHarnessSeedProviderHomeFilesWriteAndResetWipes(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	home := filepath.Join(receiver.config.DataRoot, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	receiver.config.CredentialHome = home
	gitconfig := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{
		{Path: ".claude.json", Content: `{"mcpServers":{}}`},
		{Path: ".claude/projects/-tmp-x/abc.jsonl", Content: `{"type":"summary"}`},
	}})
	if err != nil {
		t.Fatalf("seed providerHome: %v", err)
	}
	if len(result.HomeFiles) != 2 {
		t.Fatalf("HomeFiles = %v, want 2 entries", result.HomeFiles)
	}
	for _, rel := range []string{".claude.json", ".claude/projects/-tmp-x/abc.jsonl"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("seeded home file %s: %v", rel, err)
		}
	}

	for _, bad := range []string{"", ".", "..", "../outside", "/etc/passwd", `C:\evil`, ".claude/../../out"} {
		if _, err := Seed(receiver, HarnessSeedSpec{ProviderHome: []HarnessSeedHomeFile{{Path: bad, Content: "x"}}}); err == nil {
			t.Fatalf("seed providerHome path %q: no error, want traversal refusal", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(receiver.config.DataRoot, "out")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the home root (stat err %v)", err)
	}

	if err := receiver.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	for _, rel := range []string{".claude.json", ".claude"} {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Fatalf("reset left provider home state %s (stat err %v)", rel, err)
		}
	}
	if _, err := os.Stat(gitconfig); err != nil {
		t.Fatalf("reset removed .gitconfig: %v", err)
	}
}

func TestHarnessSeedWorkflowTargetValidationNamesTarget(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	_, err := receiver.seedWorkflowItems("project", HarnessSeedWorkflowItem{
		Workflow: "flow", Goal: "goal", Target: "queued",
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported target "queued"`) {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestHarnessSeedWorkflowCountIsBoundedBeforeMutation(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "too-many",
		Repo: &harness.RepoSpec{},
		Workflows: &HarnessSeedWorkflows{Items: []HarnessSeedWorkflowItem{{
			Workflow: "flow", Goal: "goal", Count: maxHarnessSeedWorkflowItems + 1,
		}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "expanded item count exceeds") {
		t.Fatalf("oversized workflow seed error = %v", err)
	}
	projects, listErr := host.store.ListProjects()
	if listErr != nil || len(projects) != 0 {
		t.Fatalf("oversized seed mutated projects: %+v, %v", projects, listErr)
	}
}

// Seeded history goes in behind the store, so the row CreateThread announced
// (IsDraft=true, no items) must be re-broadcast once the items exist. A thread
// seeded without history has nothing new to say.
func TestHarnessSeedBroadcastsRowsWhoseHistoryLandedBehindTheStore(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "broadcast-after-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{
			{Title: "with history", Turns: []HarnessSeedTurn{{
				UserText: "hi",
				Items:    []HarnessSeedItem{{Kind: "assistant_text", Summary: "Done."}},
			}}},
			{Title: "with session ref", SessionRef: "session-1"},
			{Title: "empty"},
		},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := result.Projects[0].ThreadIDs
	want := []string{ids[0], ids[1]}
	if strings.Join(host.broadcastRows, ",") != strings.Join(want, ",") {
		t.Fatalf("broadcast rows = %v, want %v", host.broadcastRows, want)
	}
	row, err := host.store.GetThread(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if row.IsDraft {
		t.Fatal("seeded thread with history still reads as a draft; the broadcast would re-announce a draft row")
	}
}

// TestHarnessSeedRepeatsTurnsAndPadsPayloads covers the two primitives a
// windowing or paging fixture needs: a thread far larger than a literal
// fixture can describe, and one payload far larger than a fixture can
// ship. Both write through the store's ordinary item path.
func TestHarnessSeedRepeatsTurnsAndPadsPayloads(t *testing.T) {
	receiver, host := newHarnessTestHost(t)
	result, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "bulk-seed",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{
			Title: "Long thread",
			Turns: []HarnessSeedTurn{
				{
					UserText: "keep sweeping",
					Items: []HarnessSeedItem{
						{Kind: "assistant_text", Summary: "swept"},
						{Kind: "assistant_text", Summary: "swept again"},
					},
					Repeat: 50,
				},
				{
					UserText: "show the log",
					Items: []HarnessSeedItem{{
						Kind:     "tool_call",
						ToolName: "Bash",
						Summary:  "go test ./...",
						Payload: &HarnessSeedPayload{
							Kind:      "tool_call_result",
							Data:      "NEEDLE the tokenizer overran the escape\n",
							PadBefore: 4096,
							PadAfter:  8192,
						},
					}},
				},
			},
		}},
	}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	threadID := result.Projects[0].ThreadIDs[0]

	items, err := host.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	// 50 repeats of (user + 2 items), then the one tool turn's user row
	// and its tool row.
	if len(items) != 50*3+2 {
		t.Fatalf("seeded %d items, want %d", len(items), 50*3+2)
	}
	// Every repeat is its own turn at its own index, in order.
	for i, item := range items[:150] {
		if item.TurnIndex != i/3 {
			t.Fatalf("item %d has turn index %d, want %d", i, item.TurnIndex, i/3)
		}
	}
	tool := items[len(items)-1]
	if tool.PayloadID == "" {
		t.Fatal("the tool row carries no payload")
	}

	data, total, _, err := host.store.GetPayloadChunk(threadID, tool.PayloadID, 0, 1<<20)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	needle := "NEEDLE the tokenizer overran the escape\n"
	if total < 4096+len(needle)+8192 {
		t.Fatalf("payload is %d bytes, want at least the padding plus the data", total)
	}
	at := strings.Index(string(data), needle)
	if at < 4096 {
		t.Fatalf("data sits at offset %d, want it after the leading padding", at)
	}
	if strings.Count(string(data), needle) != 1 {
		t.Fatalf("data appears %d times, want once", strings.Count(string(data), needle))
	}
	if !strings.HasPrefix(string(data), "pad before 000001:") {
		t.Fatalf("payload starts with %q, want the generated leading padding", string(data[:32]))
	}
	if !strings.Contains(string(data[at+len(needle):]), "pad after  000001:") {
		t.Fatal("the payload has no trailing padding after the data")
	}
	// Every filler line is one width, so the data's offset is computable.
	if want := ((4096 + seedPadLineBytes - 1) / seedPadLineBytes) * seedPadLineBytes; at != want {
		t.Fatalf("data sits at offset %d, want %d (whole padding lines)", at, want)
	}
}

func TestHarnessSeedRefusesOversizedPayloadPadding(t *testing.T) {
	receiver, _ := newHarnessTestHost(t)
	_, err := Seed(receiver, HarnessSeedSpec{Projects: []HarnessSeedProject{{
		Name: "bad-padding",
		Repo: &harness.RepoSpec{},
		Threads: []HarnessSeedThread{{Turns: []HarnessSeedTurn{{
			UserText: "hi",
			Items: []HarnessSeedItem{{
				Kind:    "tool_call",
				Summary: "Bash",
				Payload: &HarnessSeedPayload{Kind: "tool_call_result", Data: "x", PadAfter: seedPadLineLimit + 1},
			}},
		}}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("err = %v, want the padding limit refusal", err)
	}
}
