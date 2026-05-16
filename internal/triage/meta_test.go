package triage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractProposedPlanMetaIncludesStableSignatureAndTruncationFlag(t *testing.T) {
	plan := "# Phase 1\n\n## Summary\n\n" + strings.Join([]string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
		"line 7",
		"line 8",
		"line 9",
		"line 10",
		"line 11",
	}, "\n")

	meta := ExtractProposedPlanMeta(plan)

	if meta.Title != "Phase 1" {
		t.Fatalf("Title = %q, want %q", meta.Title, "Phase 1")
	}
	if !strings.HasPrefix(meta.Signature, "sha256:") {
		t.Fatalf("Signature = %q, want sha256 prefix", meta.Signature)
	}
	if !meta.PreviewTruncated {
		t.Fatalf("PreviewTruncated = false, want true")
	}
	if !strings.HasSuffix(meta.Preview, "\n\n...") {
		t.Fatalf("Preview = %q, want ellipsis suffix", meta.Preview)
	}

	changedOutsidePreview := plan + "\nline 12"
	changedMeta := ExtractProposedPlanMeta(changedOutsidePreview)
	if changedMeta.Signature == meta.Signature {
		t.Fatalf("Signature did not change when plan content changed outside preview")
	}
}

func TestExtractProposedPlanMetaMarksShortPreviewUntruncated(t *testing.T) {
	meta := ExtractProposedPlanMeta("# Short\n\nDo one thing.")

	if meta.PreviewTruncated {
		t.Fatalf("PreviewTruncated = true, want false")
	}
	if meta.Preview != "Do one thing." {
		t.Fatalf("Preview = %q, want body without title", meta.Preview)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal meta: %v", err)
	}
	if !strings.Contains(string(encoded), `"previewTruncated":false`) {
		t.Fatalf("encoded meta = %s, want explicit previewTruncated=false", encoded)
	}
}
