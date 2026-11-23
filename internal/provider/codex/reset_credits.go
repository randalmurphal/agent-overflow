package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

// Earned rate-limit resets (codex-rs app-server README §8): the
// `account/rateLimits/read` answer carries the credits an account holds in
// `rateLimitResetCredits`, and `account/rateLimitResetCredit/consume` spends
// one. See codex-wire.md#rate-limit-reset-credits.

// ResetCredits is the `rateLimitResetCredits` summary of one rate-limit read.
type ResetCredits struct {
	// AvailableCount is the authoritative number of credits available. The
	// backend may cap Credits, so it can exceed len(Credits).
	AvailableCount int
	// Credits are the credit details, nil when the backend gave only the
	// count. Entries the parser could not read are left out.
	Credits []ResetCredit
}

// ResetCredit is one earned reset.
type ResetCredit struct {
	ID string
	// ResetType is "codexRateLimits" or "unknown".
	ResetType string
	// Status is "available", "redeeming", "redeemed" or "unknown".
	Status string
	// GrantedAt and ExpiresAt are Unix seconds; ExpiresAt is 0 for a credit
	// that does not expire.
	GrantedAt   int64
	ExpiresAt   int64
	Title       string
	Description string
}

// Credit statuses the app-server reports.
const (
	ResetCreditAvailable = "available"
	ResetCreditRedeeming = "redeeming"
)

// parseResetCredits reads `rateLimitResetCredits` from a rate-limit read
// result. An absent or null summary is nil: the backend gave none. A summary
// that cannot be read is nil too, and is logged; a credit that cannot be read
// is dropped and logged.
func parseResetCredits(result json.RawMessage) *ResetCredits {
	var payload struct {
		Summary json.RawMessage `json:"rateLimitResetCredits"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil
	}
	if len(payload.Summary) == 0 || bytes.Equal(payload.Summary, []byte("null")) {
		return nil
	}
	var summary struct {
		AvailableCount *int64            `json:"availableCount"`
		Credits        []json.RawMessage `json:"credits"`
	}
	if err := json.Unmarshal(payload.Summary, &summary); err != nil || summary.AvailableCount == nil {
		log.Printf("codex: rate-limit reset credits: ignoring an unreadable summary: %s", payload.Summary)
		return nil
	}
	credits := &ResetCredits{AvailableCount: int(max(*summary.AvailableCount, 0))}
	if summary.Credits == nil {
		return credits
	}
	credits.Credits = make([]ResetCredit, 0, len(summary.Credits))
	for _, raw := range summary.Credits {
		var credit ResetCredit
		if err := json.Unmarshal(raw, &credit); err != nil || strings.TrimSpace(credit.ID) == "" {
			log.Printf("codex: rate-limit reset credits: ignoring an unreadable credit: %s", raw)
			continue
		}
		credits.Credits = append(credits.Credits, credit)
	}
	return credits
}

// UnmarshalJSON reads one credit; a null expiry is 0.
func (c *ResetCredit) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID          string  `json:"id"`
		ResetType   string  `json:"resetType"`
		Status      string  `json:"status"`
		GrantedAt   int64   `json:"grantedAt"`
		ExpiresAt   *int64  `json:"expiresAt"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = ResetCredit{
		ID:        strings.TrimSpace(wire.ID),
		ResetType: wire.ResetType,
		Status:    wire.Status,
		GrantedAt: wire.GrantedAt,
	}
	if wire.ExpiresAt != nil {
		c.ExpiresAt = *wire.ExpiresAt
	}
	if wire.Title != nil {
		c.Title = strings.TrimSpace(*wire.Title)
	}
	if wire.Description != nil {
		c.Description = strings.TrimSpace(*wire.Description)
	}
	return nil
}

// ResetSpend names one reset to spend.
type ResetSpend struct {
	// IdempotencyKey identifies one logical redemption. Sent again, it spends
	// nothing more: the answer is alreadyRedeemed.
	IdempotencyKey string
	// CreditID is the credit to spend. Every spend names one: a key sent
	// again with a named credit cannot take a different one.
	CreditID string
}

// Consume outcomes. Every one is a settled answer.
const (
	// ResetOutcomeReset: a credit was spent.
	ResetOutcomeReset = "reset"
	// ResetOutcomeAlreadyRedeemed: this idempotency key's redemption
	// completed earlier.
	ResetOutcomeAlreadyRedeemed = "alreadyRedeemed"
	// ResetOutcomeNothingToReset: no window was eligible; nothing was spent.
	ResetOutcomeNothingToReset = "nothingToReset"
	// ResetOutcomeNoCredit: the account has no credit left; nothing was spent.
	ResetOutcomeNoCredit = "noCredit"
)

var resetOutcomes = []string{
	ResetOutcomeReset, ResetOutcomeAlreadyRedeemed, ResetOutcomeNothingToReset, ResetOutcomeNoCredit,
}

// ResetRefusedError is the app-server refusing a consume before it reached
// the backend: an invalid request (a login that is not a ChatGPT sign-in, a
// malformed key), unknown parameters, or a CLI without the method. Nothing
// was spent.
type ResetRefusedError struct {
	Code    int
	Message string
}

func (e *ResetRefusedError) Error() string {
	return fmt.Sprintf("codex: rate-limit reset refused (%d): %s", e.Code, e.Message)
}

// MethodMissing reports a CLI that has no consume method.
func (e *ResetRefusedError) MethodMissing() bool { return e.Code == rpcMethodNotFound }

// ResetUnconfirmedError reports a consume whose outcome is unknown: the
// app-server's own timeout or backend failure, an unreadable answer, or a
// process that ended first. A retry with the same idempotency key spends
// nothing more.
type ResetUnconfirmedError struct {
	Cause error
}

func (e *ResetUnconfirmedError) Error() string {
	return "codex: rate-limit reset outcome unknown: " + e.Cause.Error()
}

func (e *ResetUnconfirmedError) Unwrap() error { return e.Cause }

// JSON-RPC codes the app-server answers with before calling the backend
// (codex-rs app-server/src/error_code.rs).
const (
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// classifyConsumeResponse reads the consume answer: (outcome, nil) for a
// settled outcome, *ResetRefusedError for a refusal that spent nothing, and
// *ResetUnconfirmedError for anything else.
func classifyConsumeResponse(result json.RawMessage, rpcErr *rpcError) (string, error) {
	if rpcErr != nil {
		switch rpcErr.Code {
		case rpcInvalidRequest, rpcMethodNotFound, rpcInvalidParams:
			return "", &ResetRefusedError{Code: rpcErr.Code, Message: rpcErr.Message}
		}
		return "", &ResetUnconfirmedError{Cause: fmt.Errorf("app-server error %d: %s", rpcErr.Code, rpcErr.Message)}
	}
	var payload struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", &ResetUnconfirmedError{Cause: fmt.Errorf("unreadable consume answer: %w", err)}
	}
	for _, outcome := range resetOutcomes {
		if payload.Outcome == outcome {
			return outcome, nil
		}
	}
	return "", &ResetUnconfirmedError{Cause: fmt.Errorf("unknown consume outcome %q", payload.Outcome)}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrResetSpendInvalid reports a spend without its admission hook, its send
// cutoff, an idempotency key or a credit; nothing is sent.
var ErrResetSpendInvalid = errors.New("codex: a rate-limit reset needs an admission, a send cutoff, an idempotency key and a credit")

// ErrResetLate reports a spend whose cutoff (ProbeConfig.SendBy) passed
// before its consume was written; nothing is sent.
var ErrResetLate = errors.New("codex: the reset's send cutoff passed before its consume was written")
