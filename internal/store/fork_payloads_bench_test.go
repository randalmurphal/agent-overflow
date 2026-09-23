package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

// BenchmarkForkPayloadLength compares a preview byte count read on the
// owning thread with the same read through a pointer fork's lineage arm.
func BenchmarkForkPayloadLength(b *testing.B) {
	path := filepath.Join(b.TempDir(), "store.sqlite")
	if err := copyTestStoreTemplate(path); err != nil {
		b.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	}()
	source := Thread{ID: "source", ProjectID: defaultTestProjectID, Title: "source", Provider: "claude", WorkspacePath: "/tmp"}
	if err := s.CreateThread(source); err != nil {
		b.Fatal(err)
	}
	if err := s.InsertItemWithPayload(Item{ThreadID: "source", ID: "item", Kind: "assistant_text", Role: "assistant", Status: "completed", PayloadID: "payload"}, Payload{ID: "payload", Kind: "text", Data: bytes.Repeat([]byte("x"), 32<<20)}); err != nil {
		b.Fatal(err)
	}
	if err := s.AppendPayloadData("source", "payload", bytes.Repeat([]byte("y"), 32<<20), "{}", 1); err != nil {
		b.Fatal(err)
	}
	fork := source
	fork.ID = "fork"
	if err := s.CreatePointerFork(fork, "source", ForkCut{}, func(summary string) string { return summary }, 1); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"physical", "source", "fork"} {
		b.Run(mode, func(b *testing.B) {
			for range b.N {
				if mode == "physical" {
					var n int
					if err := s.reader().QueryRow(`SELECT length(data) FROM payloads WHERE thread_id='source' AND id='payload'`).Scan(&n); err != nil || n != 32<<20 {
						b.Fatalf("length=%d: %v", n, err)
					}
				} else {
					base, _, err := s.payloadLengths(mode, "payload")
					if err != nil || base != 32<<20 {
						b.Fatalf("length=%d: %v", base, err)
					}
				}
			}
		})
	}
}
