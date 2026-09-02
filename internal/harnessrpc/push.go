// push.go — HarnessPushSent: what this backend would have sent to a phone.
//
// The harness boot swaps the LAST hop of the push path for a recorder
// (`internal/app/app_push_harness.go`). Everything before it is the
// production composition, so what this reports is the real payload: the
// same keys, the same fixed phrases, and the same absence of a thread
// title. A spec can therefore assert the redaction line rather than
// trust it.
package harnessrpc

import "fmt"

// PushMessage is one recorded wake, as the wire would have carried it.
//
// `Data` and no notification block, because the production message is
// data-only: the phone renders it, so what it says is decided by code in
// this repo rather than by Google. A spec asserting on `Data` is
// asserting on `push.MessageFor`'s contract.
type PushMessage struct {
	Token string            `json:"token"`
	Tag   string            `json:"tag"`
	Data  map[string]string `json:"data"`
}

// HarnessPushSent answers every message this boot's push fan-out produced,
// oldest first. Cleared by HarnessReset with the rest of the per-test
// ledgers.
//
// An instance with no recorder answers an EMPTY list rather than an error:
// "nothing was pushed" is a real answer and the one a spec asserting
// silence wants, and turning it into a failure would make the absence
// assertions read backwards.
func (h *Harness) HarnessPushSent() ([]PushMessage, error) {
	if h.config.Host == nil {
		return nil, fmt.Errorf("harness host unavailable")
	}
	sent := h.config.Host.PushSent()
	if sent == nil {
		return []PushMessage{}, nil
	}
	return sent, nil
}
