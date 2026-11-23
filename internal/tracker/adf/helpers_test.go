package adf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	schemaPath   = "testdata/schema-full.json"
	goldenDir    = "testdata/golden"
	corpusDir    = "testdata/corpus"
	schemaRootID = "https://example.com/adf-schema.json"
)

var (
	schemaOnce sync.Once
	schemaVal  *jsonschema.Schema
	schemaErr  error
)

func schema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	schemaOnce.Do(func() {
		f, err := os.Open(schemaPath)
		if err != nil {
			schemaErr = err
			return
		}
		defer func() { _ = f.Close() }()
		doc, err := jsonschema.UnmarshalJSON(f)
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft4)
		if err := c.AddResource(schemaRootID, doc); err != nil {
			schemaErr = err
			return
		}
		schemaVal, schemaErr = c.Compile(schemaRootID)
	})
	if schemaErr != nil {
		t.Fatalf("load schema: %v", schemaErr)
	}
	return schemaVal
}

// schemaCheck validates n against Atlassian's published JSON schema.
func schemaCheck(t testing.TB, n *Node) error {
	t.Helper()
	js, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return schemaCheckRaw(t, js)
}

type namedDoc struct {
	name string
	raw  []byte
	doc  *Node
}

func loadDocs(t testing.TB, dir string) []namedDoc {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	var out []namedDoc
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var n Node
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, namedDoc{filepath.Base(p), raw, &n})
	}
	return out
}

func mustJSON(n any) string {
	js, err := json.Marshal(n)
	if err != nil {
		panic(err)
	}
	return string(js)
}

func prettyJSON(n any) string {
	js, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(js)
}

// fakeResolver resolves mentions from a fixed table and uploads to
// predictable IDs, counting every call.
type fakeResolver struct {
	people  map[string][2]string // who -> id, display
	calls   int
	uploads []string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{people: map[string][2]string{
		"me":                     {"acc-me", "Me Myself"},
		"User 1":                 {"acc-user1", "User 1"},
		"user1@example.com":      {"acc-user1", "User 1"},
		"User 2":                 {"acc-user2", "User 2"},
		"User 4's [ops] account": {"acc-user4", "User 4's [ops] account"},
	}}
}

// resolverFor resolves every mention found in doc by its display name.
func resolverFor(doc *Node) *fakeResolver {
	r := &fakeResolver{people: map[string][2]string{}}
	walk(doc, func(n, _ *Node) {
		if n.Type == "mention" {
			name := mentionName(n)
			r.people[name] = [2]string{n.attrStr("id"), name}
		}
	})
	return r
}

func (r *fakeResolver) Mention(_ context.Context, who string) (string,
	string, error) {
	r.calls++
	p, ok := r.people[who]
	if !ok {
		return "", "", fmt.Errorf("no user matches %q", who)
	}
	return p[0], p[1], nil
}

func (r *fakeResolver) Upload(_ context.Context, path string) (string,
	error) {
	r.calls++
	if strings.Contains(path, "missing") {
		return "", fmt.Errorf("open %s: no such file", path)
	}
	r.uploads = append(r.uploads, path)
	return "upload-" + filepath.Base(path), nil
}

func para(text string) *Node {
	return &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: text}}}
}

// knownDeviation names the ways real Confluence content departs from the
// published schema: single-column layouts and breakout on rules and
// panels. Validate accepts these because the product writes them.
func knownDeviation(doc *Node) string {
	for _, b := range doc.Content {
		if b.Type == "layoutSection" && len(b.Content) < 2 {
			return "one-column layoutSection"
		}
		if (b.Type == "rule" || b.Type == "panel") && len(b.Marks) > 0 {
			return "breakout mark on " + b.Type
		}
	}
	return ""
}
func schemaCheckRaw(t testing.TB, js []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return err
	}
	return schema(t).Validate(inst)
}

// schemaAll validates many JSON documents in parallel.
func schemaAll(t testing.TB, docs [][]byte) []error {
	t.Helper()
	schema(t)
	errs := make([]error, len(docs))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				errs[i] = schemaCheckRaw(t, docs[i])
			}
		}()
	}
	for i := range docs {
		next <- i
	}
	close(next)
	wg.Wait()
	return errs
}

// checkKeptMD is checkKept of out against orig edited as md.
func checkKeptMD(orig, out *Node, md string, skip map[*Node]bool) []Warning {
	return checkKept(newEditTexts(orig, md), out, skip)
}
