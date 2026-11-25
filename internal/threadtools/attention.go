package threadtools

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// thread_attention: the calling thread flags itself as needing the user,
// with a kind and a short reason the sidebar shows on its row, until the
// user reads the thread (docs/specs/agent-thread-tools.md#thread_attention).

// The attention kinds, and AttentionNone, which lowers the flag. The store
// keeps the same three (store.AttentionDecision and the rest).
const (
	AttentionDecision = "decision"
	AttentionReview   = "review"
	AttentionProblem  = "problem"
	AttentionNone     = "none"
)

type attentionArgs struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// AttentionCall raises or lowers the caller's attention flag. Kind is one of
// the attention kinds, or AttentionNone with no Reason.
type AttentionCall struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

// AttentionAck is the flag as it stands after the call: Kind empty when it
// is lowered.
type AttentionAck struct {
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type attentionResult struct {
	Flagged bool   `json:"flagged"`
	Kind    string `json:"kind,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (c *session) attention(ctx context.Context, raw json.RawMessage) (any, error) {
	var args attentionArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if Forwarded(ctx) {
		return nil, invalidf("thread_attention flags the calling thread on its own computer and is never forwarded.")
	}
	call := AttentionCall{Kind: trim(args.Kind), Reason: trim(args.Reason)}
	switch call.Kind {
	case AttentionDecision, AttentionReview, AttentionProblem:
		if call.Reason == "" {
			return nil, invalidf("reason is required: say in a few words what needs the user.")
		}
		if strings.IndexFunc(call.Reason, notPlainLineText) >= 0 {
			return nil, invalidf("reason must be one line of plain text, with no line breaks, tabs or invisible formatting characters: the sidebar shows it on the thread's row.")
		}
		if utf8.RuneCountInString(call.Reason) > MaxAttentionReasonRunes {
			return nil, invalidf("reason must be at most %d characters.", MaxAttentionReasonRunes)
		}
	case AttentionNone:
		if call.Reason != "" {
			return nil, invalidf("kind none lowers the flag and takes no reason.")
		}
	default:
		return nil, invalidf("kind must be one of decision, review, problem or none.")
	}
	ack, err := c.app.FlagAttention(ctx, c.caller, call)
	if err != nil {
		return nil, err
	}
	return attentionResult{Flagged: ack.Kind != "", Kind: ack.Kind, Reason: ack.Reason}, nil
}

func attentionSchema() map[string]any {
	properties := map[string]any{
		"kind":   map[string]any{"type": "string", "enum": []string{AttentionDecision, AttentionReview, AttentionProblem, AttentionNone}},
		"reason": map[string]any{"type": "string", "maxLength": MaxAttentionReasonRunes, "description": "One-line sidebar label (required unless none)."},
	}
	description := "Flag thread in sidebar and notify user at turn end (required from loop/watch turns)."
	return tool("thread_attention", description, properties, "kind")
}

// notPlainLineText is a character a one-line reason may not hold: a control
// or format character (bidi overrides and zero-width characters among
// them), a surrogate, or a line or paragraph separator. The store refuses
// the same (store.CheckAttentionReason).
func notPlainLineText(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Zl, unicode.Zp)
}
