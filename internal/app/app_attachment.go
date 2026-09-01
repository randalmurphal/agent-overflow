package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Attachment BYTES do not ride the RPC wire (wave 6b). They used to: an
// upload arrived base64-encoded inside one WS frame and a full-size read
// went back the same way, so a 10 MiB image became a ~13.4 MB frame on the
// socket that also carries the live event stream.
//
// What crosses the RPC wire now is a TICKET. A client asks one of the two
// Mint methods below — scope-gated like every other bound method, and
// re-checking its own arguments rather than trusting the route to — and
// gets back a relative URL good for exactly one transfer of exactly one
// attachment. The bytes then stream over plain HTTP
// (internal/transport/attachmentroutes.go).
//
// This is not the static file server the old GetAttachmentData comment
// said we were avoiding, and the difference is worth naming: nothing under
// /attachments/ is reachable by path. A URL without a live, unspent,
// subject-matching ticket is a 404, so the thing an operator would worry
// about a file server for — a guessable path serving somebody's screenshot
// — has no way to happen.

// MintAttachmentUploadTicket authorizes ONE attachment upload and returns
// the relative URL its bytes go to.
//
// The three metadata arguments are fixed HERE, travel inside the ticket,
// and are what the stored row will say. The upload request contributes the
// bytes and nothing else, so a client cannot describe its payload as one
// thing while minting and another thing while sending.
//
// Both argument checks are deliberately duplicated with the attachment
// store's own. Refusing an oversize or disallowed payload BEFORE it is
// sent turns "10 MiB of upload, then a rejection" into one failed round
// trip, which on a phone is the whole difference; the store still checks
// again, and its signature check is the one a content type cannot talk its
// way past.
//
//ao:scope attachments:write
func (a *App) MintAttachmentUploadTicket(threadID, filename, mimeType string, sizeBytes int64) (string, error) {
	if a.attachments == nil {
		return "", fmt.Errorf("attachment store not initialized")
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", fmt.Errorf("attachment: transport is not serving")
	}
	if sizeBytes > a.attachments.MaxSize() {
		return "", fmt.Errorf("attachment: payload %d bytes exceeds limit %d", sizeBytes, a.attachments.MaxSize())
	}
	// Normalized rather than echoed: the store persists the canonical type
	// it derives, so minting with the raw one would put a value in the
	// ticket that disagrees with the row the transfer creates.
	normalizedMIME, _, err := attachment.ValidateType(mimeType, filename)
	if err != nil {
		return "", err
	}
	return server.MintAttachmentUploadTicket(threadID, filename, normalizedMIME, sizeBytes)
}

// MintAttachmentDownloadTicket authorizes ONE read of one thread-owned
// attachment and returns the relative URL its bytes come from.
//
// The thread-ownership check runs here, on metadata, before any ticket
// exists — the same check ReadThreadBytes made and for the same reason: a
// stale cross-thread id must not resolve to another thread's file. The
// route cannot make this check itself (it has no idea what an attachment
// row is), which is exactly why it has to happen at the mint.
//
//ao:scope threads:read
func (a *App) MintAttachmentDownloadTicket(threadID, attachmentID string) (string, error) {
	if a.attachments == nil {
		return "", fmt.Errorf("attachment store not initialized")
	}
	server := a.transportServer.Load()
	if server == nil {
		return "", fmt.Errorf("attachment: transport is not serving")
	}
	// Metadata only. PathForThread resolves and verifies without reading a
	// byte, so an id that names nothing — or names another thread's file —
	// costs a row lookup rather than a ticket and a wasted transfer.
	if _, _, err := a.attachments.PathForThread(threadID, attachmentID); err != nil {
		return "", err
	}
	return server.MintAttachmentDownloadTicket(threadID, attachmentID)
}

// ListAttachments returns every attachment metadata row for a thread.
//
//ao:scope threads:read
func (a *App) ListAttachments(threadID string) ([]store.Attachment, error) {
	if a.attachments == nil {
		return nil, fmt.Errorf("attachment store not initialized")
	}
	list, err := a.attachments.List(threadID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		return []store.Attachment{}, nil
	}
	return list, nil
}

// DeleteAttachment removes both the metadata row and the disk file.
//
//ao:scope attachments:write
func (a *App) DeleteAttachment(attachmentID string) error {
	if a.attachments == nil {
		return fmt.Errorf("attachment store not initialized")
	}
	return a.attachments.Delete(attachmentID)
}

// AttachmentThumbnail is the wire shape returned by GetAttachmentThumbnail.
// Carries the encoded thumbnail bytes (base64) plus the actual output mime
// type so the frontend can build a Blob without trusting the original
// attachment's mime — the thumbnailer may downconvert (e.g. WEBP source →
// JPEG output) and the consumer needs to know.
type AttachmentThumbnail struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// GetAttachmentThumbnail returns a small, inline-grid-sized version of the
// attachment, generating it lazily on first call and caching the result on
// the attachments row. ~10–30 KB per thumb vs ~1–10 MB for the original;
// the savings matter even locally (base64 + JSON encode cost) and become
// the difference between "instant grid" and "watch them load" over the
// remote `--connect` transport.
//
// This one STAYS an RPC while the full-size paths moved to HTTP, and the
// size is the whole reason. A thumbnail is ~10-30 KB, so base64 inside a
// WS frame costs a frame the socket would not notice; the round trip a
// ticket adds is pure latency, and a grid opens many of them at once. The
// rule the move was about is large bodies, and a thumbnail is not one.
//
//ao:scope threads:read
func (a *App) GetAttachmentThumbnail(threadID, attachmentID string) (AttachmentThumbnail, error) {
	if a.attachments == nil {
		return AttachmentThumbnail{}, fmt.Errorf("attachment store not initialized")
	}
	data, mime, err := a.attachments.Thumbnail(threadID, attachmentID)
	if err != nil {
		return AttachmentThumbnail{}, err
	}
	return AttachmentThumbnail{
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mime,
	}, nil
}

// AttachmentTransfer adapts the attachment store onto the transport's byte
// routes.
//
// A package function rather than a method, for the reason AuthEndpoints is
// one: an exported method on *App becomes a wire RPC by construction, and
// a seam whose entire purpose is to keep bytes OFF the RPC wire must not
// itself become one.
func AttachmentTransfer(a *App) transport.AttachmentTransfer { return attachmentTransfer{app: a} }

type attachmentTransfer struct{ app *App }

// OpenAttachment satisfies transport.AttachmentTransfer. The handle is the
// answer, not the bytes: the route streams it and closes it.
func (t attachmentTransfer) OpenAttachment(threadID, attachmentID string) (transport.AttachmentContent, error) {
	if t.app.attachments == nil {
		return transport.AttachmentContent{}, fmt.Errorf("attachment store not initialized")
	}
	// The ownership check runs AGAIN here, not only at the mint. A ticket
	// is evidence that it once passed, and the row could have moved or
	// gone since; OpenThread is where the file is actually chosen, so it
	// is where the check has to be true.
	content, err := t.app.attachments.OpenThread(threadID, attachmentID)
	if err != nil {
		return transport.AttachmentContent{}, err
	}
	return transport.AttachmentContent{
		MimeType: content.Record.MimeType,
		ModTime:  content.ModTime,
		Content:  content.File,
	}, nil
}

// StoreAttachment satisfies transport.AttachmentTransfer.
//
// The created row goes back as raw JSON because its shape belongs to
// internal/store; the transport marshals nothing of its own and learns
// nothing about what an attachment is.
func (t attachmentTransfer) StoreAttachment(req transport.AttachmentUpload) (json.RawMessage, error) {
	record, err := t.app.storeAttachment(req.ThreadID, req.Filename, req.MimeType, req.Size, req.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(record)
}

// storeAttachment persists one streamed attachment body.
//
// Unexported on purpose. This is the only write path attachments have, and
// making it a method the binding generator can see would put back the RPC
// wave 6b removed. The transfer adapter above and the package's own tests
// reach it here instead.
func (a *App) storeAttachment(threadID, filename, mimeType string, size int64, body io.Reader) (store.Attachment, error) {
	if a.attachments == nil {
		return store.Attachment{}, fmt.Errorf("attachment store not initialized")
	}
	return a.attachments.Upload(threadID, filename, mimeType, size, body, time.Now().UnixMilli())
}
