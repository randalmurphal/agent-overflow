package provider

import "testing"

func TestApprovalDeduperMarkAndIsResolved(t *testing.T) {
	d := NewApprovalDeduper(0)
	if d.IsResolved("req-1") {
		t.Fatal("fresh deduper reports req-1 resolved")
	}
	d.MarkResolved("req-1")
	if !d.IsResolved("req-1") {
		t.Fatal("after MarkResolved, req-1 should be resolved")
	}
	if d.IsResolved("req-2") {
		t.Fatal("unrelated req-2 reports resolved")
	}
}

func TestApprovalDeduperForgetReopensID(t *testing.T) {
	d := NewApprovalDeduper(0)
	d.MarkResolved("req-1")
	d.Forget("req-1")
	if d.IsResolved("req-1") {
		t.Fatal("after Forget, req-1 should not be resolved")
	}
}

func TestApprovalDeduperResetDropsAll(t *testing.T) {
	d := NewApprovalDeduper(0)
	d.MarkResolved("req-1")
	d.MarkResolved("req-2")
	d.Reset()
	if d.IsResolved("req-1") || d.IsResolved("req-2") {
		t.Fatal("after Reset, dedup set should be empty")
	}
}

func TestApprovalDeduperSoftCapResetsBeforeInsert(t *testing.T) {
	// Cap of 3: insert 3, then 4th insert should reset the set
	// (older IDs dropped) and contain only the new entry.
	d := NewApprovalDeduper(3)
	d.MarkResolved("req-1")
	d.MarkResolved("req-2")
	d.MarkResolved("req-3")
	if !d.IsResolved("req-1") {
		t.Fatal("req-1 should still be present before cap is exceeded")
	}

	d.MarkResolved("req-4")
	if d.IsResolved("req-1") || d.IsResolved("req-2") || d.IsResolved("req-3") {
		t.Fatal("after cap reset, old ids must be gone")
	}
	if !d.IsResolved("req-4") {
		t.Fatal("req-4 must be present after the reset insert")
	}
}

func TestApprovalDeduperNonPositiveCapUsesDefault(t *testing.T) {
	// A zero-value deduper (softCap=0) must still behave sanely.
	// Insertion should not infinitely reset; the field should grow
	// up to DefaultApprovalDedupCap.
	var d ApprovalDeduper
	d.MarkResolved("a")
	d.MarkResolved("b")
	if !d.IsResolved("a") || !d.IsResolved("b") {
		t.Fatal("zero-value deduper dropped entries before reaching default cap")
	}

	negative := NewApprovalDeduper(-5)
	negative.MarkResolved("a")
	negative.MarkResolved("b")
	if !negative.IsResolved("a") || !negative.IsResolved("b") {
		t.Fatal("negative cap should fall back to DefaultApprovalDedupCap")
	}
}
