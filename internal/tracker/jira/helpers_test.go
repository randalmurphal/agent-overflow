package jira

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// testToken is the credential's secret, which no answer text may repeat.
const testToken = "fake-token-6Jq2"

// newTestClient is a Client on f with a static credential.
func newTestClient(t *testing.T, f *jiratest.Server) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL: f.URL(),
		Version: "test",
		Credentials: trackerapi.StaticCredentials(
			trackerapi.Basic("user1@example.com", secret.New([]byte(testToken)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// imageDir is a new folder holding files, by slash-separated name.
func imageDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// docJSON is the ADF JSON of doc.
func docJSON(t *testing.T, doc *adf.Node) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func para(s string) *adf.Node {
	return &adf.Node{Type: "paragraph", Content: []*adf.Node{{Type: "text", Text: s}}}
}

// layoutDoc is a document with a two-column layout, which Markdown
// writes with comment markers.
func layoutDoc() *adf.Node {
	col := func(s string) *adf.Node {
		return &adf.Node{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0}, Content: []*adf.Node{para(s)}}
	}
	return adf.Doc(para("intro"), &adf.Node{Type: "layoutSection", Content: []*adf.Node{col("left"), col("right")}})
}

const (
	userOneJSON = `{"accountId":"acc-user1","displayName":"User 1","emailAddress":"user1@example.com","active":true,"accountType":"atlassian"}`
	userTwoJSON = `{"accountId":"acc-user2","displayName":"User 2","emailAddress":"user2@example.com","active":true,"accountType":"atlassian"}`
)
