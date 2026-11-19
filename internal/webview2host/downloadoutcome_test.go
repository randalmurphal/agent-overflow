package webview2host

import (
	"errors"
	"strings"
	"testing"
)

// A cancel's outcome rides any snapshot, asked or not and whatever its
// state, under its attempt, and round-trips the wire; a refusal with no
// attempt does not decode, and a running snapshot carries no reason.
func TestACancelOutcomeRidesEverySnapshot(t *testing.T) {
	refusal := CancelRefused(1, errors.New("Unspecified error"))
	for _, report := range []DownloadReport{
		{ID: "dl-1", Seq: 2, State: DownloadRunning, Answer: true, Cancel: refusal},
		{ID: "dl-1", Seq: 3, State: DownloadRunning, Cancel: refusal},
		{ID: "dl-1", Seq: 4, State: DownloadInterrupted, End: DownloadEndFailed, Reason: "the network failed", Cancel: refusal},
		{ID: "dl-1", Seq: 5, State: DownloadRunning, Cancel: CancelTaken(2)},
		{ID: "dl-1", Seq: 6, State: DownloadRunning},
	} {
		detail, err := report.Encode()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseDownloadReport(detail)
		if err != nil || parsed != report {
			t.Fatalf("%s parses as %+v, %v", detail, parsed, err)
		}
	}
	if why, refused := refusal.Refusal(); !refused || refusal.Attempt() != 1 || why != DownloadCancelRefusedReason+": Unspecified error" {
		t.Fatalf("the refusal reads %q (%v) of attempt %d", why, refused, refusal.Attempt())
	}
	if _, refused := CancelTaken(2).Refusal(); refused {
		t.Fatal("a taken cancel reads as refused")
	}
	for _, malformed := range []string{
		`{"id":"dl-1","seq":2,"state":"running","answer":true,"reason":"the browser refused to cancel the download"}`,
		`{"id":"dl-1","seq":2,"state":"running","cancel":{"refusal":"the browser refused to cancel the download"}}`,
		`{"id":"dl-1","seq":2,"state":"running","cancel":{"attempt":0}}`,
		`{"id":"dl-1","seq":2,"state":"interrupted","reason":"the network failed"}`,
		`{"id":"dl-1","seq":2,"state":"interrupted","end":"stopped","reason":"the network failed"}`,
		`{"id":"dl-1","seq":2,"state":"completed","end":"failed","path":"C:\\x.bin"}`,
	} {
		if _, err := ParseDownloadReport(malformed); err == nil {
			t.Fatalf("%s parsed", malformed)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an outcome of attempt 0 was made")
		}
	}()
	CancelTaken(0)
}

// What the launcher does with a download after each read, for each reason
// it read it: only a completed or interrupted read ends it as WebView2
// did; a running or unread one ends only at its cancel bound (running) or
// its popup's going, for that cause, and an unread one at the bound waits
// another bound.
func TestADownloadVerdictFollowsTheReadAndItsCause(t *testing.T) {
	keep := downloadVerdict{kind: verdictKeep}
	completed := downloadVerdict{kind: verdictEnd, state: DownloadCompleted, path: `C:\D\f.bin`}
	failed := downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, end: DownloadEndFailed, reason: downloadInterruptReason(12), cancelFirst: true}
	canceled := downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, end: DownloadEndCanceled, reason: downloadInterruptReason(downloadUserCanceled)}
	atBound := downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, end: DownloadEndUntracked, reason: downloadCancelBoundReason}
	gone := downloadVerdict{kind: verdictEnd, state: DownloadInterrupted, end: DownloadEndPageGone, reason: PopupGoneReason, cancelFirst: true}
	another := downloadVerdict{kind: verdictWaitAnotherBound}
	unread := observeUnread(errors.New("E_FAIL"))
	for _, tc := range []struct {
		name string
		seen downloadObservation
		want map[downloadSample]downloadVerdict
	}{
		{"running", observeRunning(), map[downloadSample]downloadVerdict{sampleChange: keep, sampleStatus: keep, sampleCancelBound: atBound, sampleRetire: gone}},
		{"completed", observeCompleted(`C:\D\f.bin`), map[downloadSample]downloadVerdict{sampleChange: completed, sampleStatus: completed, sampleCancelBound: completed, sampleRetire: completed}},
		{"failed", observeInterrupted(12), map[downloadSample]downloadVerdict{sampleChange: failed, sampleStatus: failed, sampleCancelBound: failed, sampleRetire: failed}},
		{"canceled", observeInterrupted(downloadUserCanceled), map[downloadSample]downloadVerdict{sampleChange: canceled, sampleStatus: canceled, sampleCancelBound: canceled, sampleRetire: canceled}},
		{"unread", unread, map[downloadSample]downloadVerdict{sampleChange: keep, sampleStatus: keep, sampleCancelBound: another, sampleRetire: gone}},
	} {
		for _, why := range []downloadSample{sampleChange, sampleStatus, sampleCancelBound, sampleRetire} {
			if got := tc.seen.verdict(why); got != tc.want[why] {
				t.Errorf("%s read for %d: %+v, want %+v", tc.name, why, got, tc.want[why])
			}
		}
	}
	if !strings.Contains(downloadCancelBoundReason, "no longer followed") {
		t.Fatalf("the bound's cause %q does not say the launcher stopped following the download", downloadCancelBoundReason)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an observation of no kind was decided")
		}
	}()
	downloadObservation{}.verdict(sampleStatus)
}
