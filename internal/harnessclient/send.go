package harnessclient

import (
	"context"

	"github.com/google/uuid"
)

// SendMethod is the call a page makes to send a message.
const SendMethod = "SendMessageWithOptions"

// Send sends text to a thread as the page's default send does: through
// SendMethod, with the options buildSendOptions
// (frontend/src/lib/utils/sendOptions.ts) builds for a send that consumes
// no composer draft, under a fresh send id. A send to a working thread
// queues behind its turn. Its twin is HarnessApp.send in e2e/src.
func (c *Client) Send(ctx context.Context, threadID, text string) error {
	_, err := c.Call(ctx, SendMethod, threadID, text, pageSendOptions())
	return err
}

// pageSendOptions is the options argument of one default page send. Each
// call mints a new send id: one call is one send.
func pageSendOptions() map[string]any {
	return map[string]any{
		"attachmentIds": []string{},
		"consumeDraft": map[string]any{
			"content":            "",
			"attachmentIds":      []string{},
			"terminalChips":      []any{},
			"sourceProposedPlan": nil,
		},
		"sendId":            uuid.NewString(),
		"reconcileBySendId": true,
	}
}
