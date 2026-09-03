package attachment

import (
	"fmt"

	"agent-overflow/internal/store"
)

// PromptLine is how a non-image attachment reaches the agent
// (docs/specs/file-attachments.md). Images are bound positionally to
// `[Image #N]` markers on both sides of the wire; a file has no marker and
// no slot, so it arrives as one self-describing line appended to the
// provider payload:
//
//	[Attached file "report.pdf" (application/pdf, 1.2 MB) is saved at: /…/<id>/report.pdf]
//
// The line is provider-agnostic on purpose. It goes on `providerContent`
// only — never on the persisted content or the timeline row, which carry the
// attachment in `meta` — so send, steer, queued flush, resend, the workflow
// injectors and claude-tui all deliver it without knowing it exists.
//
// absolutePath is the store's own resolved path (PathForThread), never a
// caller-composed one.
func PromptLine(record store.Attachment, absolutePath string) string {
	return fmt.Sprintf("[Attached file %q (%s, %s) is saved at: %s]",
		record.Filename, record.MimeType, FormatSize(record.Size), absolutePath)
}

// FormatSize renders a byte count the way the composer's attachment chips
// do, so the size the user saw before sending is the size the agent is told.
// One implementation, mirrored by `formatAttachmentSize` in
// frontend/src/lib/types/attachment.ts.
func FormatSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
}
