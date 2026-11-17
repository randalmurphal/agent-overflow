package webview2host

import (
	"strings"
	"testing"
	"time"
)

func TestDownloadReportRoundTrips(t *testing.T) {
	want := DownloadReport{ID: "dl-abc-1", State: DownloadCompleted, URI: "https://example.test/a.bin", Path: `C:\Users\u\Downloads\a (1).bin`, Received: 4096, Total: 4096}
	detail, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDownloadReport(detail)
	if err != nil || got != want {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, want)
	}
}

// A data: URL can be any length; the report stays within the bridge's
// detail bound by cutting the address, never the path.
func TestDownloadReportCutsALongAddressAndRefusesALongPath(t *testing.T) {
	long := DownloadReport{ID: "dl-abc-2", State: DownloadStarted, URI: "data:text/plain," + strings.Repeat("é", 4000), Path: `C:\x.txt`}
	detail, err := long.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(detail) > MaxReportDetailBytes {
		t.Fatalf("detail of %d bytes", len(detail))
	}
	parsed, err := ParseDownloadReport(detail)
	if err != nil || parsed.Path != long.Path || len(parsed.URI) > maxDownloadURIBytes || !strings.HasPrefix(long.URI, parsed.URI) {
		t.Fatalf("parsed %+v, %v", parsed, err)
	}
	if _, err := (DownloadReport{ID: "dl-abc-3", State: DownloadStarted, Path: `C:\` + strings.Repeat("a", MaxReportDetailBytes)}).Encode(); err == nil {
		t.Fatal("a path past the detail bound was encoded")
	}
}

func TestParseDownloadReportRefusesMalformedReports(t *testing.T) {
	for name, detail := range map[string]string{
		"not json":            "{",
		"no id":               `{"state":"started"}`,
		"bad id":              `{"id":"a/b","state":"started"}`,
		"unknown state":       `{"id":"dl-1","state":"paused"}`,
		"negative bytes":      `{"id":"dl-1","state":"progress","received":-1}`,
		"completed, no file":  `{"id":"dl-1","state":"completed","received":4}`,
		"negative total size": `{"id":"dl-1","state":"progress","total":-4}`,
	} {
		if _, err := ParseDownloadReport(detail); err == nil {
			t.Errorf("%s: %s parsed", name, detail)
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
