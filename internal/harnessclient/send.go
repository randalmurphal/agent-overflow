package harnessclient

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-overflow/internal/eventchan"
)

// SendMethod is the harness's send: the page's default send under a fresh
// send id (HarnessSend in internal/harnessrpc).
const SendMethod = "HarnessSend"

// sendResult is what one SendMethod call admitted only at rest did.
type sendResult struct {
	// TurnIndex is the turn the message opened.
	TurnIndex int `json:"turnIndex"`
	// CompletionsAfter is the provider:turn_completed sequence the backend
	// read before the send.
	CompletionsAfter uint64 `json:"completionsAfter"`
}

// Send sends text to a thread as the page's default send does. A send to a
// working thread queues behind its work. Its twin is HarnessApp.send in
// e2e/src.
func (c *Client) Send(ctx context.Context, threadID, text string) error {
	_, err := c.send(ctx, threadID, text, false)
	return err
}

func (c *Client) send(ctx context.Context, threadID, text string, atRestOnly bool) (sendResult, error) {
	raw, err := c.Call(ctx, SendMethod, threadID, text, atRestOnly)
	if err != nil {
		return sendResult{}, err
	}
	var result sendResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return sendResult{}, fmt.Errorf("decode %s: %w", SendMethod, err)
	}
	return result, nil
}

// SendAndAwaitTurn sends text as Send does and blocks until the turn its
// message opened completes, returning that turn's completion event.
//
// The backend admits the send only at rest, deciding under the thread's
// action lock, and answers the turn the message opened. A thread that is
// working refuses it and nothing is sent. The completion awaited is that
// turn's and sequenced after the send began: a completion announced late
// for an earlier turn, or an earlier turn that held the same index, is not
// taken for it. Completions are subscribed before the send, because a mock
// can finish a scripted turn inside the send's round trip.
func (c *Client) SendAndAwaitTurn(ctx context.Context, threadID, text string) (Event, error) {
	channel := string(eventchan.ProviderTurnCompleted)
	if err := c.Subscribe(ctx, channel); err != nil {
		return Event{}, err
	}
	sent, err := c.send(ctx, threadID, text, true)
	if err != nil {
		return Event{}, err
	}
	completed, err := c.WaitForEventOpts(ctx, channel, WaitOptions{MinSeq: sent.CompletionsAfter}, func(ev Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID  string `json:"threadId"`
			TurnIndex int    `json:"turnIndex"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID == threadID && payload.TurnIndex == sent.TurnIndex
	})
	if err != nil {
		return Event{}, fmt.Errorf("turn %d on %s never completed: %w", sent.TurnIndex, threadID, err)
	}
	return completed, nil
}
