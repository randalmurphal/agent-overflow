package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// reviewStartMethod runs Codex's built-in code review.
//
// `ReviewStart => "review/start"` with `serialization:
// thread_id(params.thread_id)`
// (codex-rs/app-server-protocol/src/protocol/common.rs, rust-v0.146.0-alpha.4),
// not `#[experimental]`. Shipped in codex 0.59.0; the `detached` delivery
// in 0.64.0. Both are below AO's 0.143 provider floor, so there is no
// runtime capability probe.
const reviewStartMethod = "review/start"

// threadCompactStartMethod asks Codex to compact the thread's context now.
//
// `ThreadCompactStart => "thread/compact/start"`, params `{threadId}`,
// response `{}` — same file, also non-experimental. Shipped in codex
// 0.96.0. The compaction BOUNDARY is not on this response: it arrives as
// the `contextCompaction` thread item, which AO already routes to
// EventCompactBoundary in protocol_item.go. The older `thread/compacted`
// notification is deprecated upstream and produces the same event.
const threadCompactStartMethod = "thread/compact/start"

// ReviewDelivery decides where the review turn runs.
//
// Wire values are camelCase (`v2_enum_from_core!` applies
// `#[serde(rename_all = "camelCase")]`; both variants are single words).
type ReviewDelivery string

const (
	// ReviewDeliveryInline runs the review on the requesting thread. This
	// is the protocol default when `delivery` is omitted.
	ReviewDeliveryInline ReviewDelivery = "inline"
	// ReviewDeliveryDetached runs the review on a NEW thread whose id comes
	// back as reviewThreadId.
	ReviewDeliveryDetached ReviewDelivery = "detached"
	// ReviewDeliveryDefault omits the field and lets Codex choose
	// (currently inline).
	ReviewDeliveryDefault ReviewDelivery = ""
)

func (d ReviewDelivery) valid() bool {
	switch d {
	case ReviewDeliveryDefault, ReviewDeliveryInline, ReviewDeliveryDetached:
		return true
	default:
		return false
	}
}

// ReviewTargetKind names the four things Codex can review.
type ReviewTargetKind string

const (
	ReviewTargetUncommittedChanges ReviewTargetKind = "uncommittedChanges"
	ReviewTargetBaseBranch         ReviewTargetKind = "baseBranch"
	ReviewTargetCommit             ReviewTargetKind = "commit"
	ReviewTargetCustom             ReviewTargetKind = "custom"
)

// ReviewTarget is the closed tagged union `review/start` takes.
//
// Its fields are unexported and it has no usable zero value on purpose:
// the four variants carry different required payloads, and a struct with
// four optional exported fields would let a caller send `{"type":"commit"}`
// with no sha. The only way to obtain a valid value is a constructor, each
// of which validates its own payload; MarshalJSON refuses anything else.
//
// Wire shape (codex-rs/app-server-protocol/src/protocol/v2/review.rs):
// `#[serde(tag = "type", rename_all = "camelCase")]` on the enum, plus
// `#[serde(rename_all = "camelCase")]` on each struct variant. So the tag
// values are the camelCased variant names and the payload keys are
// camelCase — `{"type":"baseBranch","branch":"main"}`.
type ReviewTarget struct {
	kind         ReviewTargetKind
	branch       string
	sha          string
	title        string
	instructions string
}

// Kind reports which variant this target is. It returns the empty string for a
// zero value.
func (t ReviewTarget) Kind() ReviewTargetKind { return t.kind }

func (t ReviewTarget) Branch() string       { return t.branch }
func (t ReviewTarget) SHA() string          { return t.sha }
func (t ReviewTarget) Title() string        { return t.title }
func (t ReviewTarget) Instructions() string { return t.instructions }

// ReviewUncommittedChanges reviews the working tree: staged, unstaged and
// untracked files. It takes no payload, so it cannot fail.
func ReviewUncommittedChanges() ReviewTarget {
	return ReviewTarget{kind: ReviewTargetUncommittedChanges}
}

// ReviewBaseBranch reviews the diff between the current branch and branch.
func ReviewBaseBranch(branch string) (ReviewTarget, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ReviewTarget{}, fmt.Errorf("codex: %s: base branch required", reviewStartMethod)
	}
	return ReviewTarget{kind: ReviewTargetBaseBranch, branch: branch}, nil
}

// ReviewCommit reviews the changes a single commit introduced. title is an
// optional human label for UIs; an empty title serialises as `null`, which
// is the wire's own "no label" (upstream types it `Option<String>` with no
// skip_serializing_if, so the key is always present). A commit whose label
// is the empty string and one with no label are the same fact, so the
// collapse is lossless.
func ReviewCommit(sha, title string) (ReviewTarget, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return ReviewTarget{}, fmt.Errorf("codex: %s: commit sha required", reviewStartMethod)
	}
	return ReviewTarget{kind: ReviewTargetCommit, sha: sha, title: strings.TrimSpace(title)}, nil
}

// ReviewCustom reviews against free-form instructions — the replacement
// for the old free-form review prompt.
func ReviewCustom(instructions string) (ReviewTarget, error) {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return ReviewTarget{}, fmt.Errorf("codex: %s: custom review instructions required", reviewStartMethod)
	}
	return ReviewTarget{kind: ReviewTargetCustom, instructions: instructions}, nil
}

// MarshalJSON emits the internally-tagged wire form. A zero-value target
// is an error rather than a default: "review something" has no safe
// fallback, and silently reviewing the working tree because a caller
// forgot a constructor is a worse outcome than a failed request.
func (t ReviewTarget) MarshalJSON() ([]byte, error) {
	switch t.kind {
	case ReviewTargetUncommittedChanges:
		return json.Marshal(map[string]any{"type": string(t.kind)})
	case ReviewTargetBaseBranch:
		return json.Marshal(map[string]any{"type": string(t.kind), "branch": t.branch})
	case ReviewTargetCommit:
		payload := map[string]any{"type": string(t.kind), "sha": t.sha, "title": nil}
		if t.title != "" {
			payload["title"] = t.title
		}
		return json.Marshal(payload)
	case ReviewTargetCustom:
		return json.Marshal(map[string]any{"type": string(t.kind), "instructions": t.instructions})
	default:
		return nil, fmt.Errorf("codex: %s: review target not set", reviewStartMethod)
	}
}

// UnmarshalJSON reconstructs a target from the wire form, applying the
// same payload validation the constructors do — a decoder that accepted
// `{"type":"commit"}` would reintroduce exactly the invalid state the
// unexported fields exist to prevent.
func (t *ReviewTarget) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type         string  `json:"type"`
		Branch       string  `json:"branch"`
		SHA          string  `json:"sha"`
		Title        *string `json:"title"`
		Instructions string  `json:"instructions"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("codex: decode review target: %w", err)
	}
	var (
		target ReviewTarget
		err    error
	)
	switch ReviewTargetKind(wire.Type) {
	case ReviewTargetUncommittedChanges:
		target = ReviewUncommittedChanges()
	case ReviewTargetBaseBranch:
		target, err = ReviewBaseBranch(wire.Branch)
	case ReviewTargetCommit:
		title := ""
		if wire.Title != nil {
			title = *wire.Title
		}
		target, err = ReviewCommit(wire.SHA, title)
	case ReviewTargetCustom:
		target, err = ReviewCustom(wire.Instructions)
	default:
		return fmt.Errorf("codex: decode review target: unknown type %q", wire.Type)
	}
	if err != nil {
		return err
	}
	*t = target
	return nil
}

// ReviewStarted is what `review/start` answers with.
//
// ReviewThreadID is the ROUTING AUTHORITY for everything the review
// produces — never the requested delivery, and never the session's own
// thread id. Upstream's own TUI routes on the returned id
// (tui/src/app/thread_routing.rs), and it is the id that is correct in
// both directions: inline returns the original thread, detached returns a
// freshly created one, and a future delivery mode returns whatever it
// returns.
type ReviewStarted struct {
	// ReviewThreadID identifies the thread the review runs on.
	ReviewThreadID string
	// TurnID is the review turn's id.
	TurnID string
	// TurnStatus is the turn's status at the moment the response was
	// built; the authoritative lifecycle still arrives on `turn/completed`.
	TurnStatus string
	// Detached reports whether the review actually landed on a different
	// thread than the one that asked for it. Derived from ReviewThreadID
	// rather than from the requested delivery, so a server that answers
	// differently than asked is observed rather than assumed away.
	Detached bool
}

// ReviewRunOptions carries the app-owned logical turn coordinate for the
// visible review. Codex's own review turn id is still the provider authority;
// TurnIndex only keeps the synthetic launch and result on the user command's
// already-persisted AO turn.
type ReviewRunOptions struct {
	TurnIndex int
}

type reviewStartResponse struct {
	ReviewThreadID string `json:"reviewThreadId"`
	Turn           struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

// StartReview asks Codex to run a code review without projecting it into an
// Agent Overflow logical turn. Low-level protocol callers and wire tests use
// it. User-visible reviews use StartReviewForTurn.
//
// Delivery caveat for callers: an inline review runs on the session's own
// thread and every notification it produces already routes. A DETACHED
// review runs on a thread this session does not own, so its notifications
// hit the fail-closed child-thread quarantine
// (isUnmappedForeignProviderThread) and are dropped after the bounded
// deadline. That is safe but inert — surfacing a detached review's
// transcript needs the returned ReviewThreadID registered with the
// routing tables first, which is not wired yet.
func (s *Session) StartReview(ctx context.Context, target ReviewTarget, delivery ReviewDelivery) (ReviewStarted, error) {
	return s.startReview(ctx, target, delivery, nil)
}

// StartReviewForTurn projects Codex review mode as one nested Code review
// agent. Codex exposes an outer visible review turn and a private execution
// turn. The outer id scopes UI events. The private id remains activeTurnID
// because it is the only id turn/interrupt accepts.
func (s *Session) StartReviewForTurn(
	ctx context.Context,
	target ReviewTarget,
	delivery ReviewDelivery,
	opts ReviewRunOptions,
) (ReviewStarted, error) {
	if delivery == ReviewDeliveryDetached {
		return ReviewStarted{}, fmt.Errorf("codex: %s: detached delivery has no Agent Overflow projection", reviewStartMethod)
	}
	return s.startReview(ctx, target, delivery, &opts)
}

func (s *Session) startReview(
	ctx context.Context,
	target ReviewTarget,
	delivery ReviewDelivery,
	opts *ReviewRunOptions,
) (ReviewStarted, error) {
	if !delivery.valid() {
		return ReviewStarted{}, fmt.Errorf("codex: %s: unknown delivery %q", reviewStartMethod, string(delivery))
	}
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return ReviewStarted{}, fmt.Errorf("codex: %s: session has no thread id", reviewStartMethod)
	}
	// MarshalJSON rejects an unset target, but doing it here too means the
	// caller gets the error before a request is written rather than as a
	// marshal failure inside the transport.
	encodedTarget, err := json.Marshal(target)
	if err != nil {
		return ReviewStarted{}, err
	}
	projected := opts != nil
	reserved := false
	if projected {
		if err := s.reserveReview(opts.TurnIndex, target); err != nil {
			return ReviewStarted{}, err
		}
		reserved = true
		defer func() {
			if reserved {
				s.releaseReservedReview()
			}
		}()

		model, err := s.effectiveReviewModel(ctx)
		if err != nil {
			return ReviewStarted{}, err
		}
		s.setReservedReviewModel(model)
	}
	params := map[string]any{
		"threadId": rootThreadID,
		"target":   json.RawMessage(encodedTarget),
	}
	if delivery != ReviewDeliveryDefault {
		params["delivery"] = string(delivery)
	}

	if projected {
		// The only turn/started this request produces is Codex's private review
		// execution turn. Claim it before the write so external-turn adoption does
		// not misclassify it if it beats the response.
		s.beginLocalTurnStart()
	}
	raw, err := s.sendRequest(ctx, reviewStartMethod, params)
	if err != nil {
		if projected {
			s.abandonLocalTurnStart()
			var timeout *RequestTimeoutError
			if errors.As(err, &timeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				_ = s.Close()
			}
		}
		return ReviewStarted{}, fmt.Errorf("codex: %s: %w", reviewStartMethod, err)
	}
	var resp reviewStartResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			if projected {
				_ = s.Close()
			}
			return ReviewStarted{}, fmt.Errorf("codex: %s: decode response: %w", reviewStartMethod, err)
		}
	}
	reviewThreadID := strings.TrimSpace(resp.ReviewThreadID)
	if reviewThreadID == "" {
		// The whole routing contract rests on this field. An empty one
		// would silently fall back to "assume inline", which is the exact
		// assumption the returned id exists to replace.
		if projected {
			_ = s.Close()
		}
		return ReviewStarted{}, fmt.Errorf("codex: %s: response carried no reviewThreadId", reviewStartMethod)
	}
	if projected && reviewThreadID != rootThreadID {
		// A projected inline review on another thread cannot be scoped or
		// interrupted through this Session. Close the process so the billed
		// review cannot continue invisibly after we report the protocol fault.
		_ = s.Close()
		return ReviewStarted{}, fmt.Errorf(
			"codex: %s: inline review landed on detached thread %s",
			reviewStartMethod, reviewThreadID,
		)
	}
	if projected {
		if err := s.bindReviewResponse(resp.Turn.ID); err != nil {
			_ = s.Close()
			return ReviewStarted{}, err
		}
		reserved = false
	}
	return ReviewStarted{
		ReviewThreadID: reviewThreadID,
		TurnID:         strings.TrimSpace(resp.Turn.ID),
		TurnStatus:     strings.TrimSpace(resp.Turn.Status),
		Detached:       reviewThreadID != rootThreadID,
	}, nil
}

// CompactThread asks Codex to compact this session's thread context now.
//
// The response body is empty; the observable effect is the
// `contextCompaction` thread item, which flows through the existing
// EventCompactBoundary path and lands as the transcript's compaction
// divider. Like a review, compaction runs as a non-steerable turn
// (`turnKind: "compact"`), so callers should gate it on the thread being
// idle rather than racing a live turn.
func (s *Session) CompactThread(ctx context.Context) error {
	s.mu.Lock()
	busy := s.turn.activeTurnID != "" || s.review != nil
	s.mu.Unlock()
	if busy {
		return fmt.Errorf("codex: %s: thread already has an active turn", threadCompactStartMethod)
	}
	rootThreadID := s.rootThreadID()
	if rootThreadID == "" {
		return fmt.Errorf("codex: %s: session has no thread id", threadCompactStartMethod)
	}
	if _, err := s.sendRequest(ctx, threadCompactStartMethod, map[string]any{
		"threadId": rootThreadID,
	}); err != nil {
		return fmt.Errorf("codex: %s: %w", threadCompactStartMethod, err)
	}
	return nil
}
