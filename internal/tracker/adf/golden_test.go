package adf

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden outputs")

// Golden files in testdata/golden, by name prefix:
//
//	rt-X.md + rt-X.json        both directions
//	write-X.md + write-X.json  FromMarkdown only
//	read-X.json + read-X.md    ToMarkdown only
//	err-X.md + err-X.err       FromMarkdown fails with this message
//	edit-X.orig.json + edit-X.md + edit-X.json   Edit
//
// An optional X.warn lists expected warnings, one per line, a refusal
// (Flattens) marked "refused: ". With -update
// the outputs are rewritten from the current code, for review.
func TestGolden(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(goldenDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range paths {
		base := filepath.Base(p)
		base = strings.TrimSuffix(base, ".orig.json")
		names[strings.TrimSuffix(base, filepath.Ext(base))] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	if len(sorted) == 0 {
		t.Fatal("no golden files")
	}
	for _, name := range sorted {
		t.Run(name, func(t *testing.T) { runGolden(t, name) })
	}
}

func goldenPath(name, ext string) string {
	return filepath.Join(goldenDir, name+ext)
}

func readGolden(t *testing.T, name, ext string) string {
	t.Helper()
	b, err := os.ReadFile(goldenPath(name, ext))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeGolden(t *testing.T, name, ext, content string) {
	t.Helper()
	if err := os.WriteFile(goldenPath(name, ext), []byte(content),
		0o644); err != nil {
		t.Fatal(err)
	}
}

func parseGoldenJSON(t *testing.T, s string) *Node {
	t.Helper()
	var n Node
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func warnText(ws []Warning) string {
	var b strings.Builder
	for _, w := range ws {
		if w.Flattens {
			if !namesContent(w) {
				b.WriteString("refused for dress, a bug: ")
			}
			b.WriteString("refused: ")
		}
		b.WriteString(w.String() + "\n")
	}
	return b.String()
}

// collectionResolver is a fake Confluence resolver.
type collectionResolver struct{ *fakeResolver }

func (collectionResolver) Collection() string { return "contentId-42" }

func goldenResolver(name string) Resolver {
	if strings.Contains(name, "noresolver") {
		return nil
	}
	if strings.Contains(name, "confluence") {
		return collectionResolver{newFakeResolver()}
	}
	return newFakeResolver()
}

func runGolden(t *testing.T, name string) {
	ctx := context.Background()
	kind, _, _ := strings.Cut(name, "-")
	checkWarn := func(ws []Warning) {
		t.Helper()
		got := warnText(ws)
		if *update {
			if got != "" {
				writeGolden(t, name, ".warn", got)
			} else {
				_ = os.Remove(goldenPath(name, ".warn"))
			}
			return
		}
		want := ""
		if b, err := os.ReadFile(goldenPath(name, ".warn")); err == nil {
			want = string(b)
		}
		if got != want {
			t.Errorf("warnings:\ngot:\n%swant:\n%s", got, want)
		}
	}
	checkDoc := func(doc *Node) {
		t.Helper()
		if err := Validate(doc); err != nil {
			t.Errorf("Validate: %v", err)
		}
		if err := schemaCheck(t, doc); err != nil {
			t.Errorf("schema: %v", err)
		}
		got := prettyJSON(doc) + "\n"
		if *update {
			writeGolden(t, name, ".json", got)
			return
		}
		if want := readGolden(t, name, ".json"); got != want {
			t.Errorf("FromMarkdown:\ngot:\n%s\nwant:\n%s", got, want)
		}
	}
	switch kind {
	case "rt", "write":
		md := readGolden(t, name, ".md")
		doc, ws, err := FromMarkdown(ctx, md, goldenResolver(name))
		if err != nil {
			t.Fatalf("FromMarkdown: %v", err)
		}
		checkDoc(doc)
		checkWarn(ws)
		if kind == "rt" {
			if got := ToMarkdown(doc); got != md {
				t.Errorf("ToMarkdown:\ngot:\n%s\nwant:\n%s", got, md)
			}
		}
	case "read":
		doc := parseGoldenJSON(t, readGolden(t, name, ".json"))
		got := ToMarkdown(doc)
		if *update {
			writeGolden(t, name, ".md", got)
		} else if want := readGolden(t, name, ".md"); got != want {
			t.Errorf("ToMarkdown:\ngot:\n%s\nwant:\n%s", got, want)
		}
		// Unchanged Markdown gives back a valid doc exactly. (Some read
		// goldens hold node types Validate rejects.)
		if Validate(doc) == nil {
			checkEditIdentity(t, doc, got)
		}
		back, _, err := FromMarkdown(ctx, got, goldenResolver(name))
		if err != nil {
			t.Errorf("rendered Markdown does not parse: %v", err)
		} else if err := Validate(back); err != nil {
			t.Errorf("rendered Markdown parses to invalid ADF: %v", err)
		} else if !strings.Contains(name, "lossy") &&
			semanticKey(back.Content) != semanticKey(doc.Content) {
			t.Errorf("rendered Markdown does not round trip:\n%s",
				prettyJSON(back))
		}
	case "err":
		md := readGolden(t, name, ".md")
		_, _, err := FromMarkdown(ctx, md, goldenResolver(name))
		if err == nil {
			t.Fatal("FromMarkdown succeeded, want an error")
		}
		if *update {
			writeGolden(t, name, ".err", err.Error()+"\n")
		} else if want := strings.TrimSpace(readGolden(t, name,
			".err")); err.Error() != want {
			t.Errorf("error:\ngot:  %s\nwant: %s", err, want)
		}
		if strings.HasPrefix(name, "err-resolver") {
			var le *lineError
			if !errors.As(err, &le) || le.Unwrap() == nil {
				t.Errorf("resolver error is not wrapped: %#v", err)
			}
		}
	case "edit":
		orig := parseGoldenJSON(t, readGolden(t, name, ".orig.json"))
		md := readGolden(t, name, ".md")
		r := newFakeResolver()
		doc, ws, err := Edit(ctx, orig, md, r)
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		checkDoc(doc)
		checkWarn(ws)
	default:
		t.Fatalf("unknown golden kind %q", kind)
	}
}

func checkEditIdentity(t *testing.T, doc *Node, md string) {
	t.Helper()
	edited, _, err := Edit(context.Background(), doc, md, resolverFor(doc))
	if err != nil {
		t.Errorf("Edit with the rendered Markdown: %v", err)
	} else if mustJSON(edited) != mustJSON(doc) {
		t.Errorf("Edit with the rendered Markdown changed the doc:\n%s",
			prettyJSON(edited))
	}
}
