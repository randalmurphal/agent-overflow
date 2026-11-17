package webview2host

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDownloadReportRoundTrips(t *testing.T) {
	want := DownloadReport{ID: "dl-abc-1", Seq: 7, Answer: true, State: DownloadCompleted, URI: "https://example.test/a.bin?x=1&y=<2>", Name: "a (1).bin", Path: `C:\Users\u\Downloads\a (1).bin`, Received: 4096, Total: 4096}
	detail, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, `\u0026`) || strings.Contains(detail, `\u003c`) {
		t.Fatalf("the report escapes HTML: %s", detail)
	}
	got, err := ParseDownloadReport(detail)
	if err != nil || got != want {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, want)
	}
}

// The address is optional: whatever it holds, it is shortened on a
// character boundary or left out until the report fits, and the path and
// the rest of the snapshot are kept whole.
func TestDownloadReportShortensItsAddressToFit(t *testing.T) {
	for _, tc := range []struct {
		name string
		uri  string
	}{
		{"the reviewer's ampersands", "data:text/plain," + strings.Repeat("&", 1000)},
		{"100,000 bytes", "https://example.test/" + strings.Repeat("a", 100_000)},
		{"multi-byte characters", "data:text/plain," + strings.Repeat("é€😀", 2000)},
		{"escaped quotes and controls", "data:text/plain," + strings.Repeat("\"\x01\\", 2000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := DownloadReport{ID: "dl-abc-2", Seq: 1, State: DownloadRunning, URI: tc.uri, Name: "x.txt", Path: `C:\Users\u\Downloads\x.txt`, Received: 1, Total: 9}
			detail, err := report.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if len(detail) > MaxReportDetailBytes {
				t.Fatalf("detail of %d bytes", len(detail))
			}
			parsed, err := ParseDownloadReport(detail)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(parsed.URI) || !strings.HasPrefix(tc.uri, parsed.URI) {
				t.Fatalf("the address was not cut on a character boundary: %q", parsed.URI[max(len(parsed.URI)-8, 0):])
			}
			parsed.URI = report.URI
			if parsed != report {
				t.Fatalf("parsed %+v, want %+v apart from the address", parsed, report)
			}
		})
	}
}

// A path that cannot fit is left out, never cut: a running snapshot keeps
// the name, and a completed one says why it names no file. A path near the
// bound fits whole.
func TestDownloadReportLeavesOutAPathThatCannotFit(t *testing.T) {
	long := `C:\` + strings.Repeat("a", MaxReportDetailBytes) + `\x.txt`
	for _, state := range []string{DownloadRunning, DownloadCompleted} {
		detail, err := DownloadReport{ID: "dl-abc-3", Seq: 1, State: state, URI: "https://example.test/x", Name: "x.txt", Path: long, Received: 5}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseDownloadReport(detail)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Path != "" || parsed.Name != "x.txt" || parsed.Received != 5 {
			t.Fatalf("%s: parsed %+v", state, parsed)
		}
		if state == DownloadCompleted && parsed.Reason != DownloadPathTooLongReason {
			t.Fatalf("a completed snapshot without its path says %q", parsed.Reason)
		}
	}
	base := DownloadReport{ID: "dl-abc-4", Seq: 1, State: DownloadCompleted, Name: "x.txt", Received: 5, Total: 5}
	empty, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The longest path that fits leaves nothing out; JSON doubles each
	// backslash.
	near := `C:\` + strings.Repeat("b", MaxReportDetailBytes-len(empty)-len(`,"path":""`)-len(`C:\\`)-len(`\\x.txt`)) + `\x.txt`
	base.Path = near
	detail, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseDownloadReport(detail); err != nil || parsed.Path != near || len(detail) != MaxReportDetailBytes {
		t.Fatalf("a path at the bound: %d bytes, %v, path kept: %v", len(detail), err, parsed.Path == near)
	}
}

func TestParseDownloadReportRefusesMalformedReports(t *testing.T) {
	for name, detail := range map[string]string{
		"not json":                    "{",
		"no id":                       `{"seq":1,"state":"running"}`,
		"bad id":                      `{"id":"a/b","seq":1,"state":"running"}`,
		"unknown state":               `{"id":"dl-1","seq":1,"state":"paused"}`,
		"negative bytes":              `{"id":"dl-1","seq":1,"state":"running","received":-1}`,
		"completed, no file, no word": `{"id":"dl-1","seq":1,"state":"completed","received":4}`,
		"negative total size":         `{"id":"dl-1","seq":1,"state":"running","total":-4}`,
		"a snapshot with no number":   `{"id":"dl-1","state":"running"}`,
		"a numbered unknown":          `{"id":"dl-1","seq":2,"answer":true,"state":"unknown"}`,
		"an unknown not answering":    `{"id":"dl-1","state":"unknown"}`,
	} {
		if _, err := ParseDownloadReport(detail); err == nil {
			t.Errorf("%s: %s parsed", name, detail)
		}
	}
}

// A page keeps its last MaxEndedDownloads terminal snapshots to answer a
// directive, as answers under their own numbers; past the bound the oldest
// is answered as unknown.
func TestEndedDownloadsAreBoundedAndAnswerUnknownIDs(t *testing.T) {
	var ended endedDownloads
	for i := range MaxEndedDownloads + 3 {
		ended.add(DownloadReport{ID: fmt.Sprintf("dl-%d", i), Seq: 4, State: DownloadCompleted, Path: `C:\x`})
	}
	if len(ended.byID) != MaxEndedDownloads || len(ended.order) != MaxEndedDownloads {
		t.Fatalf("kept %d snapshots in %d places", len(ended.byID), len(ended.order))
	}
	if got := ended.answer("dl-3"); got.State != DownloadCompleted || got.Path != `C:\x` || got.Seq != 4 || !got.Answer {
		t.Fatalf("a kept snapshot answers %+v", got)
	}
	for _, id := range []string{"dl-0", "dl-nope"} {
		if got := ended.answer(id); got != (DownloadReport{ID: id, State: DownloadUnknown, Reason: DownloadUnknownReason, Answer: true}) {
			t.Fatalf("%s answers %+v", id, got)
		}
	}
}

// Progress is reported at most once per interval, so a fast download
// cannot flood the bridge; the first report always goes.
func TestProgressGatePassesOneReportPerInterval(t *testing.T) {
	var gate progressGate
	start := time.Unix(1000, 0)
	if !gate.due(start) {
		t.Fatal("the first progress report was held")
	}
	passed := 0
	for i := 1; i <= 1000; i++ {
		if gate.due(start.Add(time.Duration(i) * time.Millisecond)) {
			passed++
		}
	}
	if want := int(time.Second / DownloadProgressInterval); passed != want {
		t.Fatalf("a second of per-millisecond chunks passed %d reports, want %d", passed, want)
	}
}

func TestDownloadInterruptReasonNamesEveryKind(t *testing.T) {
	if got := downloadInterruptReason(12); got != "the network failed" {
		t.Fatalf("NETWORK_FAILED = %q", got)
	}
	if got := downloadInterruptReason(downloadUserCanceled); got != "the download was canceled" {
		t.Fatalf("USER_CANCELED = %q", got)
	}
	if got := downloadInterruptReason(29); got != "the download process crashed" {
		t.Fatalf("DOWNLOAD_PROCESS_CRASHED = %q", got)
	}
	if got := downloadInterruptReason(99); !strings.Contains(got, "99") {
		t.Fatalf("an unknown reason = %q", got)
	}
}
