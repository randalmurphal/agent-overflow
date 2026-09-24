package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Include heavy tool output: transcript row counts alone understate fork work.
func BenchmarkForkHistory(b *testing.B) {
	for _, fixture := range []struct {
		count  int
		nested bool
	}{{1000, false}, {10000, false}, {15000, true}} {
		count := fixture.count
		name := fmt.Sprint(count)
		if fixture.nested {
			name += "/nested"
		}
		b.Run(name, func(b *testing.B) {
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
			source := Thread{ID: "source", ProjectID: defaultTestProjectID, Title: "Long thread", Provider: "claude", WorkspacePath: "/tmp", CreatedAt: 1, UpdatedAt: 1}
			if err := s.CreateThread(source); err != nil {
				b.Fatal(err)
			}
			tx, err := s.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Rollback()
			data := strings.Repeat("tool output\n", 3000)
			for i := range count {
				id := fmt.Sprint(i)
				if _, err := tx.Exec(`INSERT INTO payloads(thread_id,id,kind,data,created_at) VALUES ('source',?,'text',?,1)`, id, data); err != nil {
					b.Fatal(err)
				}
				parent, kind, meta := "", "assistant_text", "{}"
				if fixture.nested {
					meta = `{"provider_item_id":"fixture","detail":"` + strings.Repeat("metadata ", 50) + `"}`
					if i%64 == 0 {
						kind = "tool_call"
					} else {
						parent = fmt.Sprint(i - i%64)
					}
				}
				if _, err := tx.Exec(`INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,payload_id,parent_id,meta,created_at,updated_at) VALUES ('source',?,?,? ,?,'assistant','completed','tool output',?,?,?,1,1)`, id, i/256, i%256, kind, id, parent, meta); err != nil {
					b.Fatal(err)
				}
			}
			for index := 0; index <= (count-1)/256; index++ {
				var completed any = 2
				if index == (count-1)/256 {
					completed = nil
				}
				if _, err := tx.Exec(`INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at) VALUES(?,'source',?,1,?)`, fmt.Sprintf("source:%d", index), index, completed); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for range b.N {
				fork := BuildForkedThread(source)
				fork.ForkPreparing = true
				if err := s.CreatePointerFork(fork, source.ID, ForkCut{}, func(summary string) string { return summary + " interrupted" }, 3); err != nil {
					b.Fatal(err)
				}
				if _, err := s.FinishForkPreparation(fork.ID); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := s.DeleteThread(fork.ID); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.StopTimer()
		})
	}
}
