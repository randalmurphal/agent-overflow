package highlightapp

import (
	"strings"
	"testing"

	"agent-overflow/internal/highlight"
)

const testPatch = "diff --git a/demo.go b/demo.go\n--- a/demo.go\n+++ b/demo.go\n@@ -1 +1 @@\n-old\n+new\n"

func TestComputePatchSpanSeedsBoundsAndPriming(t *testing.T) {
	service := New(Config{})
	seeds := service.computePatchSpanSeeds(testPatch, func(path string) string {
		if path == "demo.go" {
			return "new\n"
		}
		return ""
	})
	if len(seeds) != 1 || !seeds[0].Primed || seeds[0].ContentKey == "" {
		t.Fatalf("seeds = %+v", seeds)
	}
	drifted := service.computePatchSpanSeeds(testPatch, func(string) string { return "different\n" })
	if len(drifted) != 1 || drifted[0].Primed {
		t.Fatalf("drifted = %+v", drifted)
	}
	invalid := strings.Replace(testPatch, "+new", "+\xff", 1)
	if got := service.computePatchSpanSeeds(invalid, nil); got != nil {
		t.Fatalf("invalid seeds = %+v", got)
	}
	over := strings.Replace(testPatch, "+new", "+"+strings.Repeat("x", diffSeedMaxFileBytes), 1)
	if got := service.computePatchSpanSeeds(over, nil); got != nil {
		t.Fatalf("oversize seeds = %+v", got)
	}
}

func TestCapPatchSpanSeedBytesSkipsLargeWithoutStarvingLater(t *testing.T) {
	seed := func(path string, lines int) PatchSpanSeed {
		out := PatchSpanSeed{Path: path}
		for range lines {
			out.Lines = append(out.Lines, highlight.EncodedLine{})
		}
		return out
	}
	got := capPatchSpanSeedBytes([]PatchSpanSeed{seed("a", 1), seed("big", 10), seed("c", 1)}, 16)
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "c" {
		t.Fatalf("got = %+v", got)
	}
}

func TestObserveDiffPayloadDropsPastWorkerCap(t *testing.T) {
	service := New(Config{})
	service.diffWorkers.Store(diffSeedMaxWorkers)
	service.ObserveDiffPayload("thread", "", "payload", nil, testPatch)
	if got := service.diffWorkers.Load(); got != diffSeedMaxWorkers {
		t.Fatalf("workers = %d", got)
	}
}
