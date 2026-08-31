package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// syncThreadWindowTimeout bounds the read transaction SyncThreadWindow
// opens. On a WAL store the query runs on the read pool under snapshot
// isolation and never queues behind the writer; a store without one
// (:memory:, a non-WAL fallback) serves reads from the writer
// connection, where this deadline is what stops a cold open from
// waiting on a long write. It also covers the WAL pathology (a wedged
// pool, a checkpoint holding the read pool quiesced) where holding the
// RPC open serves nobody. Matches markThreadReadTimeout and the store's
// busy_timeout.
const syncThreadWindowTimeout = 5 * time.Second

// SyncThreadWindowRequest is the client's cold-open question: "here is
// the window I have cached and the stamps I believe describe it — is it
// still current?" (docs/architecture/thread-replica-sync.md §5).
//
// HaveEpoch / HaveRev are -1 when the caller holds no replica or does not
// trust its stamp. -1 can never equal a real stamp, so the answer is
// always a page — understating is the client's safe direction (§3.4).
type SyncThreadWindowRequest struct {
	// AnchorItemID positions the window exactly as ListThreadSliceAround
	// does: empty means the tail, otherwise the caller's saved scroll
	// anchor (an anchor that no longer exists falls back to the tail).
	AnchorItemID string `json:"anchorItemId"`
	// ItemBudget is the target window size; <= 0 takes the default and
	// oversized requests are capped, same as every other history binding.
	ItemBudget int   `json:"itemBudget"`
	HaveEpoch  int64 `json:"haveEpoch"`
	HaveRev    int64 `json:"haveRev"`
	// InlinePreviews is the client's stated projection preference: true
	// when it paints inline diff previews on arrival, false when they
	// sit behind a chevron (`collapseDiffPreviews`, the default) and
	// none of the patch text is rendered until clicked. It rides the
	// request because it is a per-CLIENT setting and one backend serves
	// several clients that can disagree; the server never reads the
	// setting itself.
	InlinePreviews bool `json:"inlinePreviews,omitempty"`
}

// SyncThreadWindowResponse is the answer. Page is nil for "fresh"
// (nothing changed — this is the ~100-byte response the whole design
// exists for) and for "gone" (no such thread).
//
// Epoch / Rev describe EXACTLY the returned page: both were read in the
// same transaction as the rows, so a client may adopt them the moment it
// has applied the page.
type SyncThreadWindowResponse struct {
	// Status is one of "fresh", "stale", "rewritten", "gone" — see
	// store.SyncStatus for what each obliges the client to do.
	Status     string            `json:"status"`
	Epoch      int64             `json:"epoch"`
	Rev        int64             `json:"rev"`
	Generation string            `json:"generation"`
	Page       *store.PagedItems `json:"page,omitempty"`
}

// SyncThreadWindow is the cold-open replacement for
// ListThreadSliceAround: it answers with the window only when the
// caller's stamps prove it necessary. Store-read-only — it opens one
// read-pool transaction and touches no local FS, process, or credential
// state, so it is deliberately NOT in transport.LocalOnlyMethods.
//
// The other paging RPCs are unchanged; this one covers the initial
// window only.
func (a *App) SyncThreadWindow(threadID string, req SyncThreadWindowRequest) (SyncThreadWindowResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), syncThreadWindowTimeout)
	defer cancel()

	result, err := a.store.SyncThreadWindow(ctx, threadID, req.AnchorItemID, clampSliceItemBudget(req.ItemBudget), store.HistoryStamp{
		Rev:   req.HaveRev,
		Epoch: req.HaveEpoch,
	})
	if err != nil {
		return SyncThreadWindowResponse{}, normalizeThreadWindowSyncError(ctx, err)
	}

	out := SyncThreadWindowResponse{
		Status:     string(result.Status),
		Epoch:      result.Stamp.Epoch,
		Rev:        result.Stamp.Rev,
		Generation: result.Generation,
	}
	if result.Page != nil {
		// Same window as ListThreadSliceAround, so the same projection
		// and the same byte backstop. A cold open that reached this RPC
		// and a gap refresh that reached that one must not disagree
		// about how a row is shaped, or one window would hold both.
		page := projectPage(*result.Page, req.InlinePreviews, keepNewest)
		out.Page = &page
	}
	return out, nil
}

func normalizeThreadWindowSyncError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf(
			"%w: thread history read timed out after %s: %w",
			transport.ErrTemporarilyUnavailable,
			syncThreadWindowTimeout,
			ctxErr,
		)
	}
	return fmt.Errorf("sync thread window: %w", err)
}
