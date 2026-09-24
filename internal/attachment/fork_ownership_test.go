package attachment

import (
	"fmt"
	"os"
	"testing"

	"agent-overflow/internal/store"
)

func TestForkAttachmentsSurviveOriginalAndIntermediateDeletion(t *testing.T) {
	for _, file := range []bool{false, true} {
		for _, generated := range []bool{false, true} {
			t.Run(fmt.Sprint("file=", file, "/generated=", generated), func(t *testing.T) {
				s, meta := newTestStores(t)
				for _, id := range []string{"source", "fork", "grandchild", "foreign"} {
					seedThread(t, meta, id)
				}
				data, name, mime := []byte("document bytes"), "report.txt", "text/plain"
				if !file {
					data = realPNG(t)
					name = "image.png"
					mime = "image/png"
				}
				a, err := uploadBytes(s, "source", name, mime, data, 1)
				if err != nil {
					t.Fatal(err)
				}
				_, originalPath, err := s.PathForThread("source", a.ID)
				if err != nil {
					t.Fatal(err)
				}
				item := store.Item{ThreadID: "source", ID: "prompt", Kind: "user_text", Role: "user", Status: "completed", Summary: "attachment", Meta: fmt.Sprintf(`{"attachments":[{"id":%q,"threadId":"source","filename":%q,"mimeType":%q,"size":%d}]}`, a.ID, name, mime, len(data)), CreatedAt: 1, UpdatedAt: 1}
				// A generated reference arrives through imported history.
				if generated {
					item.Kind = "assistant_text"
					item.Role = "assistant"
					if err := meta.ApplyImportBatch("source", store.ImportBatch{Rows: []store.ImportRow{{Item: item}}}); err != nil {
						t.Fatal(err)
					}
				} else if err := meta.InsertItem(item); err != nil {
					t.Fatal(err)
				}
				for _, pair := range [][2]string{{"source", "fork"}, {"fork", "grandchild"}} {
					if _, err := meta.CloneThreadHistoryThroughTurn(pair[0], pair[1], nil); err != nil {
						t.Fatal(err)
					}
				}
				if _, _, err := s.PathForThread("foreign", a.ID); err == nil {
					t.Fatal("unrelated thread gained attachment access")
				}
				for _, id := range []string{"source", "fork"} {
					if err := s.DeleteThreadDir(id); err != nil {
						t.Fatal(err)
					}
					if err := meta.DeleteThread(id); err != nil {
						t.Fatal(err)
					}
				}
				record, path, err := s.PathForThread("grandchild", a.ID)
				if err != nil {
					t.Fatal(err)
				}
				if path != originalPath || record.ThreadID != "grandchild" {
					t.Fatalf("native path or logical owner changed: %s %+v", path, record)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(data) {
					t.Fatal("attachment bytes changed")
				}
				if !file {
					if _, _, err := s.Thumbnail("grandchild", a.ID); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.DeleteThreadDir("grandchild"); err != nil {
					t.Fatal(err)
				}
				if err := meta.DeleteThread("grandchild"); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("last owner retained attachment file: %v", err)
				}
				if _, found, err := meta.GetAttachment(a.ID); err != nil || found {
					t.Fatalf("last owner retained metadata: found=%v err=%v", found, err)
				}
			})
		}
	}
}
