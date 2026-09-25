package attachment

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"agent-overflow/internal/store"
)

// TestForkAttachmentsFollowTheHistoryThatShowsThem pins attachment access
// through pointer forks. A fork and a fork of it read the attachment a
// source message carries without owning it, and a thread outside the
// lineage cannot. The file belongs to the source's history: a delete of
// the source or of the fork between keeps it, with the message, as the
// holder the grandchild reads, at its native path. The grandchild's delete
// releases the holders, and their deletes remove the file.
func TestForkAttachmentsFollowTheHistoryThatShowsThem(t *testing.T) {
	for _, file := range []bool{false, true} {
		for _, generated := range []bool{false, true} {
			t.Run(fmt.Sprint("file=", file, "/generated=", generated), func(t *testing.T) {
				s, meta := newTestStores(t)
				for _, id := range []string{"source", "foreign"} {
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
					fork, err := meta.GetThread(pair[0])
					if err != nil {
						t.Fatal(err)
					}
					fork.ID, fork.ForkedFromThreadID = pair[1], pair[0]
					if err := meta.CreatePointerFork(fork, pair[0], store.ForkCut{}, func(summary string) string { return summary }, 2); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range []string{"fork", "grandchild"} {
					if _, path, err := s.PathForThread(id, a.ID); err != nil || path != originalPath {
						t.Fatalf("%s reads %q, %v; want the source's file", id, path, err)
					}
					if owned, err := meta.ListAttachments(id); err != nil || len(owned) != 0 {
						t.Fatalf("%s owns %d attachments err=%v; a pointer fork owns none", id, len(owned), err)
					}
				}
				if _, _, err := s.PathForThread("foreign", a.ID); err == nil {
					t.Fatal("unrelated thread gained attachment access")
				}
				deleteThread := func(id string) {
					t.Helper()
					if err := s.DeleteThreadDir(id); err != nil {
						t.Fatal(err)
					}
					if err := meta.DeleteThread(id); err != nil {
						t.Fatal(err)
					}
				}
				for _, id := range []string{"source", "fork"} {
					deleteThread(id)
				}
				_, path, err := s.PathForThread("grandchild", a.ID)
				if err != nil || path != originalPath {
					t.Fatalf("grandchild reads %q, %v; want the source's file", path, err)
				}
				if record, found, err := meta.GetAttachment(a.ID); err != nil || !found || record.ThreadID != "source" {
					t.Fatalf("attachment record = %+v found=%v err=%v; want the holder's", record, found, err)
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
				deleteThread("grandchild")
				released, err := meta.ListReleasedHolders()
				if err != nil {
					t.Fatal(err)
				}
				slices.Sort(released)
				if !slices.Equal(released, []string{"fork", "source"}) {
					t.Fatalf("released holders = %v, want fork and source", released)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("a released holder's file went before its delete: %v", err)
				}
				for _, id := range released {
					deleteThread(id)
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
