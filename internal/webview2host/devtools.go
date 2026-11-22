package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
)

// DevToolsReport is the detail of a ReportDevToolsOpened or
// ReportDevToolsFailed: the OpDevTools request it answers, and a failure's
// error.
type DevToolsReport struct {
	Request uint64 `json:"request"`
	Error   string `json:"error,omitempty"`
}

// Encode answers the report as a report detail.
func (r DevToolsReport) Encode() string {
	data, err := json.Marshal(r)
	if err != nil {
		// A string and an integer always encode.
		panic(err)
	}
	return string(data)
}

// ParseDevToolsReport decodes a DevTools report's detail.
func ParseDevToolsReport(detail string) (DevToolsReport, error) {
	var r DevToolsReport
	if err := json.Unmarshal([]byte(detail), &r); err != nil {
		return DevToolsReport{}, fmt.Errorf("decode devtools report: %w", err)
	}
	if r.Request == 0 {
		return DevToolsReport{}, errors.New("devtools report names no request")
	}
	return r, nil
}
