package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/loopback"
	"agent-overflow/internal/transferwire"
)

const ThreadTransferPrefix = "/transfers/"

// ThreadTransferEndpoints adapts the application coordinator. Authorization is
// a live lookup of a grant bound to this ONE operation, never a page cookie or
// an exported device credential. Each mutation serializes on the operation and
// rechecks its durable phase before touching files or publishing history.
type ThreadTransferEndpoints interface {
	Authorize(context.Context, string, string) bool
	Status(context.Context, string) (transferwire.State, error)
	BeginUpload(context.Context, string, transferwire.Upload) error
	ReceiveChunk(context.Context, string, int64, int64, string, io.Reader) error
	Prepare(context.Context, string) error
	Activate(context.Context, string, []byte) error
	Cancel(context.Context, string, []byte) error
}

func (s *Server) handleThreadTransfer(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, ThreadTransferPrefix), "/")
	if len(parts) != 2 || !entityid.Valid(parts[0]) || r.URL.RawQuery != "" ||
		(r.TLS == nil && !loopback.PeerAddress(r.RemoteAddr)) {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		http.NotFound(w, r)
		return
	}
	grant := strings.TrimPrefix(values[0], "Bearer ")
	if _, err := transferwire.DecodeSecret(grant); err != nil || !s.cfg.ThreadTransfers.Authorize(r.Context(), id, grant) {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(transferwire.VersionHeader) != "1" {
		s.replyThreadTransfer(w, id, nil, "unsupported_transfer_version", http.StatusConflict)
		return
	}
	var backendID string
	if s.cfg.BackendIdentity != nil {
		backendID, _ = s.cfg.BackendIdentity()
	}
	if backendID == "" {
		s.replyThreadTransfer(w, id, nil, "transfer_unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get(transferwire.BackendHeader) != backendID {
		s.replyThreadTransfer(w, id, nil, "destination_changed", http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if action == "status" && r.Method == http.MethodGet {
		state, err := s.cfg.ThreadTransfers.Status(r.Context(), id)
		s.finishThreadTransfer(w, id, &state, err)
		return
	}
	if (action == "chunk" && r.Method != http.MethodPut) || (action != "chunk" && r.Method != http.MethodPost) {
		s.replyThreadTransfer(w, id, nil, "transfer_invalid", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	controller := http.NewResponseController(w)
	// Read deadlines interrupt a socket blocked inside Body.Read; checking a
	// context before Read alone cannot interrupt a stalled upload.
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Minute))
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	var err error
	switch action {
	case "upload":
		var upload transferwire.Upload
		if err = decodeTransferControl(w, r, &upload); err == nil {
			if !transferwire.ValidDigest(upload.SHA256) || upload.Size < 1024 || upload.Size > transferwire.MaxUploadBytes || upload.Size%512 != 0 {
				err = transferwire.ErrInvalid
			} else {
				err = s.cfg.ThreadTransfers.BeginUpload(ctx, id, upload)
			}
		}
	case "chunk":
		offset, parseErr := strconv.ParseInt(r.Header.Get(transferwire.OffsetHeader), 10, 64)
		digest := r.Header.Get(transferwire.DigestHeader)
		if parseErr != nil || offset < 0 || r.ContentLength < 1 || r.ContentLength > transferwire.MaxChunkBytes || !transferwire.ValidDigest(digest) {
			err = transferwire.ErrInvalid
		} else {
			err = s.cfg.ThreadTransfers.ReceiveChunk(ctx, id, offset, r.ContentLength, digest, http.MaxBytesReader(w, r.Body, r.ContentLength))
		}
	case "activate", "cancel":
		var request transferwire.Activation
		if err = decodeTransferControl(w, r, &request); err == nil {
			var secret []byte
			if secret, err = transferwire.DecodeSecret(request.Secret); err == nil {
				if action == "activate" {
					err = s.cfg.ThreadTransfers.Activate(ctx, id, secret)
				} else {
					err = s.cfg.ThreadTransfers.Cancel(ctx, id, secret)
				}
			}
		}
	case "prepare":
		var empty struct{}
		if err = decodeTransferControl(w, r, &empty); err == nil {
			err = s.cfg.ThreadTransfers.Prepare(ctx, id)
		}
	default:
		err = transferwire.ErrInvalid
	}
	var state transferwire.State
	if err == nil {
		state, err = s.cfg.ThreadTransfers.Status(ctx, id)
	}
	s.finishThreadTransfer(w, id, &state, err)
}

func decodeTransferControl(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(target); err != nil {
		return transferwire.ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return transferwire.ErrInvalid
	}
	return nil
}

func (s *Server) finishThreadTransfer(w http.ResponseWriter, id string, state *transferwire.State, err error) {
	if err == nil {
		s.replyThreadTransfer(w, id, state, "", http.StatusOK)
		return
	}
	code, status := "transfer_failed", http.StatusInternalServerError
	switch {
	case errors.Is(err, transferwire.ErrInvalid):
		code, status = "transfer_invalid", http.StatusBadRequest
	case errors.Is(err, transferwire.ErrConflict):
		code, status = "transfer_conflict", http.StatusConflict
	case errors.Is(err, transferwire.ErrNotReady):
		code, status = "transfer_not_ready", http.StatusConflict
	case errors.Is(err, transferwire.ErrUnavailable):
		code, status = "transfer_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code, status = "transfer_timeout", http.StatusRequestTimeout
	default:
		log.Printf("transfer %s: %v", id, err)
	}
	s.replyThreadTransfer(w, id, nil, code, status)
}

func (s *Server) replyThreadTransfer(w http.ResponseWriter, id string, state *transferwire.State, code string, status int) {
	var backendID string
	if s.cfg.BackendIdentity != nil {
		backendID, _ = s.cfg.BackendIdentity()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(transferwire.Reply{Version: transferwire.Version, BackendID: backendID, OperationID: id, State: state, Error: code})
}
