package harnessrpc

import (
	"fmt"
	"strings"
)

// HarnessRequestWaits answers, from the thread tools' wait registry itself,
// which of the given request tokens a tool call holds at this moment: a
// thread_status wait parked on it, or the spawn, send, ask or new assistant
// that made it, from before its message is dispatched until its response is
// written. The result names every token given. A spec orders "the call is
// waiting" before "the responder may reply" on it; nothing persisted, such
// as an armed wake keeping an Agents row listed, answers that question.
func (h *Harness) HarnessRequestWaits(tokens []string) (map[string]bool, error) {
	if h.config.Host == nil {
		return nil, fmt.Errorf("harness host unavailable")
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("at least one request token is required")
	}
	trimmed := make([]string, 0, len(tokens))
	for index, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, fmt.Errorf("request token %d is blank", index)
		}
		trimmed = append(trimmed, token)
	}
	return h.config.Host.RequestWaits(trimmed), nil
}
