package webview2host

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPopupReportRoundTrips(t *testing.T) {
	want := PopupReport{Opener: "0123abcd", Target: "ABCDEF0123", URL: "https://example.test/a?b=<c>&d"}
	detail, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "<c>&d") {
		t.Errorf("the address was escaped: %s", detail)
	}
	got, err := ParsePopupReport(detail)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip %+v, want %+v", got, want)
	}
}

// Every report fits MaxReportDetailBytes, which the launcher's report
// channel cuts at, with its address shortened on a character boundary.
func TestPopupReportShortensItsAddressToFit(t *testing.T) {
	for name, url := range map[string]string{
		"long":             "https://example.test/" + strings.Repeat("a", 3*MaxPopupURLBytes),
		"multibyte":        "https://example.test/" + strings.Repeat("é", MaxPopupURLBytes),
		"escaped sixfold":  strings.Repeat("\x01", MaxPopupURLBytes),
		"escaped twofold":  strings.Repeat(" ", MaxPopupURLBytes),
		"invalid encoding": strings.Repeat("\xff", MaxPopupURLBytes),
	} {
		t.Run(name, func(t *testing.T) {
			detail, err := PopupReport{Opener: "page", Target: "target", URL: url}.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if len(detail) > MaxReportDetailBytes {
				t.Fatalf("a %d-byte report", len(detail))
			}
			got, err := ParsePopupReport(detail)
			if err != nil {
				t.Fatal(err)
			}
			if got.URL == "" || len(got.URL) > MaxPopupURLBytes || !utf8.ValidString(got.URL) {
				t.Errorf("address of %d bytes", len(got.URL))
			}
		})
	}
}

func TestParsePopupReportRefusesMalformedReports(t *testing.T) {
	for name, detail := range map[string]string{
		"not json":           "popup",
		"no opener":          `{"target":"t"}`,
		"opener not an id":   `{"opener":"a/b","target":"t"}`,
		"target not a token": `{"opener":"p","target":"t\n"}`,
		"address too long":   `{"opener":"p","target":"t","url":"` + strings.Repeat("a", MaxPopupURLBytes+1) + `"}`,
	} {
		if _, err := ParsePopupReport(detail); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// A popup that went before its target was read is reported with none.
	detail, err := (PopupReport{Opener: "p"}).Encode()
	if err != nil {
		t.Fatalf("a report with no target: %v", err)
	}
	if got, err := ParsePopupReport(detail); err != nil || got.Target != "" {
		t.Errorf("a report with no target parsed as %+v, %v", got, err)
	}
}

// A popup's page id is a valid page id that no backend page id, which is hex
// digits alone, can equal.
func TestPopupPageIDsAreValidAndApart(t *testing.T) {
	id := popupPageID("0123456789ab", 1<<63)
	if err := ValidatePageID(id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(id, "-") {
		t.Errorf("popup id %q has no dash", id)
	}
	if popupPageID("t", 1) == popupPageID("t", 2) || popupPageID("a", 1) == popupPageID("b", 1) {
		t.Error("two popups share an id")
	}
}
