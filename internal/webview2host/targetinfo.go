package webview2host

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ParseTargetID pulls targetInfo.targetId out of a Target.getTargetInfo
// response.
//
// This is the handle the backend attaches chromedp to. WebView2 has no
// /json/new, so a page can only be driven by explicit target id
// (chromedp.WithTargetID) — which means a controller whose target id we
// failed to read is a page the backend cannot use, and the host reports
// create-failed rather than created.
//
// Cross-platform on purpose: the parse is the part that can be wrong, and
// keeping it out of the COM file is what lets the Linux suite cover it.
func ParseTargetID(resultJSON string) (string, error) {
	var response struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &response); err != nil {
		return "", fmt.Errorf("decode Target.getTargetInfo response: %w", err)
	}
	if response.TargetInfo.TargetID == "" {
		return "", errors.New("Target.getTargetInfo returned no targetId")
	}
	return response.TargetInfo.TargetID, nil
}
