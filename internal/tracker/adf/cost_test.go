package adf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cost benchmarks measure a ticket description's conversions at
// realistic sizes, from the hand-written corpus: decoding the stored
// document, rendering it to Markdown (every Detail and comment read),
// parsing Markdown back (a create), and applying an edit to the stored
// document (every rich text edit: Edit, CheckLoss, then encoding the
// document to send).

func corpusDoc(tb testing.TB, name string) ([]byte, *Node) {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus", name))
	if err != nil {
		tb.Fatal(err)
	}
	var doc Node
	if err := json.Unmarshal(raw, &doc); err != nil {
		tb.Fatal(err)
	}
	return raw, &doc
}

var costDocs = []string{"doc-24.json", "doc-16.json"}

func BenchmarkDecodeDoc(b *testing.B) {
	for _, name := range costDocs {
		raw, _ := corpusDoc(b, name)
		b.Run(fmt.Sprintf("%s-%dKB", name, len(raw)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				var doc Node
				if err := json.Unmarshal(raw, &doc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkToMarkdown(b *testing.B) {
	for _, name := range costDocs {
		raw, doc := corpusDoc(b, name)
		b.Run(fmt.Sprintf("%s-%dKB", name, len(raw)>>10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if ToMarkdown(doc) == "" {
					b.Fatal("empty")
				}
			}
		})
	}
}

func BenchmarkFromMarkdown(b *testing.B) {
	for _, name := range costDocs {
		raw, doc := corpusDoc(b, name)
		md := ToMarkdown(doc)
		// A document that mentions people needs a resolver to parse back;
		// the benchmark measures the documents that parse without one.
		if _, _, err := FromMarkdown(context.Background(), md, nil); err != nil {
			b.Logf("%s: skipped, needs a resolver: %v", name, err)
			continue
		}
		b.Run(fmt.Sprintf("%s-%dKB-md%dKB", name, len(raw)>>10, len(md)>>10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := FromMarkdown(context.Background(), md, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEditOneParagraph is an edit that changes one sentence of the
// rendered Markdown, applied to the stored document, checked for loss
// and encoded, as a description edit is before it is sent.
func BenchmarkEditOneParagraph(b *testing.B) {
	for _, name := range costDocs {
		raw, doc := corpusDoc(b, name)
		md := ToMarkdown(doc)
		i := strings.Index(md, "\n\n")
		if i < 0 {
			b.Fatal("no paragraph break")
		}
		edited := md[:i] + " Edited." + md[i:]
		b.Run(fmt.Sprintf("%s-%dKB", name, len(raw)>>10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out, ws, err := Edit(context.Background(), doc, edited, nil)
				if err != nil {
					b.Fatal(err)
				}
				if err := CheckLoss(ws, true); err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
