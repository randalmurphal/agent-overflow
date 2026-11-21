package browser

import (
	"testing"
	"time"

	jsonv2 "github.com/chromedp/cdproto/cdp/jsonv2"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// chromedp.Console joins the console API, uncaught exceptions and the
// browser log in one ordered stream. The log keeps what it showed before
// the join: no exceptions, the page's URL for an API call, the entry's own
// URL for a log entry, and level log for a verbose log entry.
func TestConsoleEntryKeepsTheLogFormat(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	pageURL := func() string { return "https://page.test/" }
	cases := []struct {
		name    string
		message chromedp.ConsoleMessage
		want    ConsoleLog
		wantOK  bool
	}{
		{"an API call", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleWarning, Time: at, URL: "https://script.test/app.js",
			Args: []*cdpruntime.RemoteObject{
				{Type: "string", Value: jsonv2.Value(`"hello"`)}, {Type: "number", Value: jsonv2.Value(`3`)},
				{Type: "object", Description: "Window"}, {Type: "undefined"},
			},
		}, ConsoleLog{Level: "warn", Message: "hello 3 Window undefined", Timestamp: at.Format(time.RFC3339Nano), URL: "https://page.test/"}, true},
		{"a log entry", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleError, Text: "Failed to load resource", Time: at, URL: "https://cdn.test/a.png", Source: "network",
		}, ConsoleLog{Level: "error", Message: "Failed to load resource", Timestamp: at.Format(time.RFC3339Nano), URL: "https://cdn.test/a.png"}, true},
		{"a verbose log entry", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleDebug, Text: "verbose", Time: at, Source: "other",
		}, ConsoleLog{Level: "log", Message: "verbose", Timestamp: at.Format(time.RFC3339Nano)}, true},
		{"a console.debug call", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleDebug, Time: at, Args: []*cdpruntime.RemoteObject{{Type: "string", Value: jsonv2.Value(`"d"`)}},
		}, ConsoleLog{Level: "debug", Message: "d", Timestamp: at.Format(time.RFC3339Nano), URL: "https://page.test/"}, true},
		{"an uncaught exception", chromedp.ConsoleMessage{
			Type: chromedp.ConsoleException, Text: "Uncaught Error: boom", Time: at, Exception: &cdpruntime.ExceptionDetails{Text: "Uncaught"},
		}, ConsoleLog{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := consoleEntry(tc.message, pageURL)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("consoleEntry = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
