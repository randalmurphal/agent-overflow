// Package planrevision owns the pure helpers behind the proposed-plan
// inline-comment revision flow: rendering selected comments as a
// prompt block the agent can read, and projecting a comment slice
// into its ID list.
//
// The App-bound CRUD methods (`CreateProposedPlanComment`,
// `UpdateProposedPlanComment`, `DeleteProposedPlanComment`), the
// `SendPlanRevisionComments` saga that flips a thread back into plan
// mode and dispatches the revision, the `appendPlanRevisionCommentsToContent`
// composer in `app_send.go`, the selected-text resolver, and the
// proposed-plan-upsert emitter all stay in `app_proposed_plans.go`.
package planrevision

import (
	"strings"

	"agent-overflow/internal/store"
)

// BuildPrompt renders draft proposed-plan comments into a prompt
// block the agent can read directly. Each comment becomes:
//
//	<selected_text>
//	comment: <body>
//
// Multiple comments are separated by a blank line. Empty
// selected-text plus empty body is skipped — the agent has the full
// plan already in the same thread, so we only emit comments that
// add new context.
func BuildPrompt(comments []store.ProposedPlanComment) string {
	var b strings.Builder
	for _, comment := range comments {
		selectedText := strings.TrimSpace(comment.SelectedText)
		body := strings.TrimSpace(comment.Body)
		if selectedText == "" && body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if selectedText != "" {
			b.WriteString(selectedText)
			b.WriteString("\n")
		}
		b.WriteString("comment: ")
		b.WriteString(body)
	}
	return b.String()
}

// IDsOf projects a comment slice into its ID list, preserving order.
func IDsOf(comments []store.ProposedPlanComment) []string {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ID)
	}
	return ids
}
