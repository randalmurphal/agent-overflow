package harnessclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
)

// SendMethod is the call a page makes to send a message.
const SendMethod = "SendMessageWithOptions"

// ThreadWorkMethod answers which threads still run or owe a turn: the check
// ThreadAtRest makes before a send whose turn is awaited.
const ThreadWorkMethod = "HarnessThreadsWorking"

// Send sends text to a thread as the page's default send does: through
// SendMethod, with the options buildSendOptions
// (frontend/src/lib/utils/sendOptions.ts) builds for a send that consumes
// no composer draft, under a fresh send id. A send to a working thread
// queues behind its turn. Its twin is HarnessApp.send in e2e/src.
func (c *Client) Send(ctx context.Context, threadID, text string) error {
	return c.send(ctx, threadID, uuid.NewString(), text)
}

func (c *Client) send(ctx context.Context, threadID, sendID, text string) error {
	_, err := c.Call(ctx, SendMethod, threadID, text, pageSendOptions(sendID))
	return err
}

// pageSendOptions is the options argument of one default page send under
// sendID. pageDefaultSend.json in frontend/src/test/fixtures is the
// reference it is checked against.
func pageSendOptions(sendID string) map[string]any {
	return map[string]any{
		"attachmentIds": []string{},
		"consumeDraft": map[string]any{
			"content":            "",
			"attachmentIds":      []string{},
			"terminalChips":      []any{},
			"sourceProposedPlan": nil,
		},
		"sendId":            sendID,
		"reconcileBySendId": true,
	}
}

// RestingThread is a thread ThreadAtRest found with nothing running or
// owed, for one send whose turn SendAndAwaitTurn waits on.
type RestingThread struct {
	c        *Client
	threadID string
}

// ThreadAtRest refuses a thread that is working: an open turn, a queued
// message, a prompt waiting for an answer, or a compaction. A send to such
// a thread queues behind that work, and nothing the backend publishes names
// the turn a queued message runs in: a Claude message flushed while a turn
// runs keeps that turn's index on its row whether the turn takes it in or
// it runs as a turn of its own. A caller with work to arm before the send
// checks first, so a refusal leaves nothing to undo.
func (c *Client) ThreadAtRest(ctx context.Context, threadID string) (RestingThread, error) {
	var working []struct {
		ActiveTurn   bool   `json:"activeTurn"`
		PendingSends bool   `json:"pendingSends"`
		WaitingOn    string `json:"waitingOn"`
		Compacting   bool   `json:"compacting"`
	}
	raw, err := c.Call(ctx, ThreadWorkMethod, []string{threadID})
	if err != nil {
		return RestingThread{}, err
	}
	if err := json.Unmarshal(raw, &working); err != nil {
		return RestingThread{}, fmt.Errorf("decode %s: %w", ThreadWorkMethod, err)
	}
	if len(working) == 0 {
		return RestingThread{c: c, threadID: threadID}, nil
	}
	var doing []string
	w := working[0]
	if w.ActiveTurn {
		doing = append(doing, "a turn is open")
	}
	if w.PendingSends {
		doing = append(doing, "a message is queued or starting its turn")
	}
	if w.WaitingOn != "" {
		doing = append(doing, "it waits on an "+w.WaitingOn)
	}
	if w.Compacting {
		doing = append(doing, "it is compacting")
	}
	return RestingThread{}, fmt.Errorf("thread %s is working (%s): a message sent now would queue behind that work, and the turn it runs in cannot be named; wait for the thread to rest, or send without waiting",
		threadID, strings.Join(doing, ", "))
}

// SendAndAwaitTurn sends text as Send does and blocks until the turn its
// message opened completes, returning that turn's completion event.
//
// The send id is minted here and the wait for the message's row parked
// before the send: a mock can finish a scripted turn inside the send round
// trip, so a wait registered after the call returns would miss it. The row
// that carries the send id names the turn; the completion awaited is that
// turn's, not the first one the thread reports. A completion announced
// late for an earlier turn, or a turn something else started between
// ThreadAtRest and the send, is not taken for this message's.
func (t RestingThread) SendAndAwaitTurn(ctx context.Context, text string) (Event, error) {
	c := t.c
	itemChannel := string(eventchan.ProviderItemEvent)
	turnChannel := string(eventchan.ProviderTurnCompleted)
	if err := c.Subscribe(ctx, itemChannel, turnChannel); err != nil {
		return Event{}, err
	}
	sendID := uuid.NewString()
	rowAwait := c.Await(itemChannel, func(ev Event) bool {
		_, ok := sentRowTurn(ev, t.threadID, sendID)
		return ok
	})
	if err := c.send(ctx, t.threadID, sendID, text); err != nil {
		rowAwait.Close()
		return Event{}, err
	}
	row, err := rowAwait.Wait(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("the message sent to %s never appeared as a row: %w", t.threadID, err)
	}
	turnIndex, _ := sentRowTurn(row, t.threadID, sendID)
	completed, err := c.WaitForEvent(ctx, turnChannel, func(ev Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID  string `json:"threadId"`
			TurnIndex int    `json:"turnIndex"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID == t.threadID && payload.TurnIndex == turnIndex
	})
	if err != nil {
		return Event{}, fmt.Errorf("turn %d on %s never completed: %w", turnIndex, t.threadID, err)
	}
	return completed, nil
}

// sentRowTurn reports the turn of the user row an item upsert carries when
// that row is the one written for sendID on threadID.
func sentRowTurn(ev Event, threadID, sendID string) (int, bool) {
	if ev.Gap {
		return 0, false
	}
	var upsert struct {
		Action   string `json:"action"`
		ThreadID string `json:"threadId"`
		Item     *struct {
			Kind      string `json:"kind"`
			TurnIndex int    `json:"turnIndex"`
			Meta      string `json:"meta"`
		} `json:"item"`
	}
	if json.Unmarshal(ev.Data, &upsert) != nil || upsert.Action != "upsert" || upsert.ThreadID != threadID ||
		upsert.Item == nil || upsert.Item.Kind != "user_text" || upsert.Item.Meta == "" {
		return 0, false
	}
	var meta struct {
		SendID string `json:"sendId"`
	}
	if json.Unmarshal([]byte(upsert.Item.Meta), &meta) != nil || meta.SendID != sendID {
		return 0, false
	}
	return upsert.Item.TurnIndex, true
}
