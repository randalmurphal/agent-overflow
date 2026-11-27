package trackerapp

import "encoding/json"

// This file stands in for the credit section of Credit change 1, which has
// not landed: countsCredit reads the section's "on" from the raw section
// this build keeps unread. Change 1 reads the section and owns the
// accessor; at its merge this file is deleted and the evidence unit calls
// change 1's accessor.

// countsCredit reports that the setup counts credit: its credit section
// is on. No section counts none.
func (h *heldSetup) countsCredit() bool {
	raw, ok := h.raw["credit"]
	if !ok {
		return false
	}
	var section struct {
		On bool `json:"on"`
	}
	return json.Unmarshal(raw, &section) == nil && section.On
}
