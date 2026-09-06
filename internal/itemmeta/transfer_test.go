package itemmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTransferAttachmentReferencesPreserveOtherMetadata(t *testing.T) {
	destinations := map[string]AttachmentDestination{"attachment-old": {SourceThreadID: "parent", ThreadID: "destination", ID: "attachment-new"}}
	raw := `{"provider_item_id":"wire-id","large":9007199254740993,"text":"attachment-old parent","attachments":[{"id":"attachment-old","threadId":"parent","filename":"photo.png","future":{"nested":1}}]}`
	got, err := TransferAttachments(raw, "source", destinations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"large":9007199254740993`) || !strings.Contains(got, `"text":"attachment-old parent"`) || !strings.Contains(got, `"provider_item_id":"wire-id"`) {
		t.Fatalf("rewrote unrelated metadata: %s", got)
	}
	var row struct {
		Attachments []struct {
			ID, ThreadID, Filename string
			Future                 map[string]int
		}
	}
	if err := json.Unmarshal([]byte(got), &row); err != nil {
		t.Fatal(err)
	}
	if len(row.Attachments) != 1 || row.Attachments[0].ID != "attachment-new" || row.Attachments[0].ThreadID != "destination" || row.Attachments[0].Future["nested"] != 1 {
		t.Fatalf("lost attachment metadata: %s", got)
	}
	if _, err := TransferAttachments(strings.Replace(raw, `"threadId":"parent"`, `"threadId":"wrong"`, 1), "source", destinations); err == nil {
		t.Fatal("borrowed another thread's attachment")
	}
	if _, err := TransferAttachments(raw, "source", nil); err == nil {
		t.Fatal("silently lost missing attachment")
	}
	if got, err := TransferAttachmentArray(`["attachment-old"]`, "source", destinations); err != nil || got != `["attachment-new"]` {
		t.Fatalf("legacy draft: %s %v", got, err)
	}
}

func TestTransferThreadReferencesKeepExternalLinksAndProse(t *testing.T) {
	raw := `{"provider_item_id":"wire","text":"source note-1","revisionSourceProposedPlan":{"threadId":"source","itemId":"plan"},"revisionSourceCommentIds":["note-1"],"revisionSourceDiffReview":{"threadId":"external","sourceKey":"review"},"revisionSourceDiffCommentIds":["external-note"]}`
	got, err := TransferThreadReferences(raw, "source", "destination", func(kind, id string) string { return kind + ":" + id })
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"threadId":"destination"`, `"plan_comment:note-1"`, `"threadId":"external"`, `"external-note"`, `"text":"source note-1"`, `"provider_item_id":"wire"`} {
		if !strings.Contains(got, expected) {
			t.Errorf("lost %s: %s", expected, got)
		}
	}
}
