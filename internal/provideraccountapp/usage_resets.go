package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// Usage resets: the resets a Codex account holds are kept on the account row
// (provideraccounts.UsageResets), written by every reading attributed to the
// account that spoke about them, and spent by SpendProviderAccountReset. A
// spend runs inside the usage transaction (usageTransaction), under the
// provider's reconcile lock, and that transaction's reading after the spend
// is what the card then shows.
//
// A spend is recorded with its key immediately before its consume is written
// (admitResetSpend, provideraccounts.PendingReset), published once the write
// was attempted, and stays recorded until Codex settles a request under that
// key, which it does on receipt of the answer (settleAnsweredResetSpend).
// While it stands the account offers Finish (FinishProviderAccountReset)
// instead of its resets.

// ResetSpendResult is the settled answer to a spend or a Finish.
type ResetSpendResult struct {
	// Message says what happened, in words for the person who asked.
	Message string `json:"message"`
	// Spent reports that the account's limits were reset by this spend or by
	// the earlier request it finished.
	Spent bool `json:"spent"`
	// ReadingLanded reports that the reading after the spend was recorded,
	// so the account's usage is current.
	ReadingLanded bool `json:"readingLanded"`
}

// UnsettledReset is the card's view of an account's unsettled spend. The key
// and the credit stay in the store.
type UnsettledReset struct {
	// ID names the spend for Finish (FinishProviderAccountReset).
	ID      string `json:"id"`
	Label   string `json:"label"`
	Refills string `json:"refills,omitempty"`
	// StartedAt is Unix milliseconds.
	StartedAt int64 `json:"startedAt"`
}

func unsettledReset(pending provideraccounts.PendingReset, ok bool) *UnsettledReset {
	if !ok {
		return nil
	}
	return &UnsettledReset{ID: pending.ID, Label: pending.Label, Refills: pending.Refills, StartedAt: pending.StartedAt}
}

// resetSpendRequest is a spend the usage transaction carries out: a fresh
// spend of optionID from the list at revision, or with finish the unsettled
// spend spendID sent again.
type resetSpendRequest struct {
	finish   bool
	optionID string
	revision uint64
	spendID  string
	// sendBy is the cutoff fixed when the call arrived. Past it nothing is
	// sent.
	sendBy time.Time

	// Filled by the transaction. pending is the record the consume went out
	// under, nil until admitResetSpend wrote or found it; outcome is Codex's
	// answer to the consume, and settleErr the failure of the save that
	// settled it.
	pending   *provideraccounts.PendingReset
	outcome   string
	settleErr error
	result    ResetSpendResult
}

// goneCreditID is the credit Codex's answer says is used or gone, "" when
// there is no answer, no spend, or the answer kept it (nothingToReset).
func (s *resetSpendRequest) goneCreditID() string {
	if s == nil || s.pending == nil || s.outcome == "" || s.outcome == codex.ResetOutcomeNothingToReset {
		return ""
	}
	return s.pending.CreditID
}

// ResetSendCutoff bounds everything a spend does before its consume: the
// lock wait, the temporary home, the app-server's start, the account/read and
// the record. Past it the spend is refused unsent. With
// codex.ResetSettleTimeout after the write, a spend ends inside its method
// budget, so a caller never gives up on a spend that then goes out.
const ResetSendCutoff = 30 * time.Second

// ResetLockWait bounds how long a spend waits for the provider's reconcile
// lock behind another account operation. Past it the spend is refused unsent.
const ResetLockWait = 10 * time.Second

// The sentences a spend's refusals and failures show. They are reviewed
// prose (errorsx.Public), so a client shows each whole.
var (
	// ErrResetListChanged refuses a spend chosen from a list that is no
	// longer the account's: the person has not seen what it would spend.
	ErrResetListChanged = errorsx.Public("reset_list_changed",
		"The resets for this account changed since they were shown. Check them and try again.", nil)
	errResetUnconfirmed = errorsx.Public("reset_unconfirmed",
		"Codex did not confirm the reset. Press Finish on the account to complete it; Finish never uses a second reset.", nil)
	errResetCodexTooOld = errorsx.Public("reset_refused",
		"This version of Codex can't use resets. Update Codex and try again. No reset was used.", nil)
	errResetNothingUnsettled = errorsx.Public("reset_settled",
		"This reset is already settled, so nothing was sent.", nil)
	errResetFinishOther = errorsx.Public("reset_finish_changed",
		"The unconfirmed reset on this account isn't the one that was shown, so Finish wasn't sent. Check the account and try again.", nil)
)

// SpendProviderAccountReset spends one of an account's usage resets, chosen
// from the list at revision, then reads the account's usage again. It is
// refused unsent while the account has an unsettled spend.
func (m *Manager) SpendProviderAccountReset(
	ctx context.Context,
	providerName, accountID, optionID string,
	revision uint64,
) (ResetSpendResult, error) {
	return m.spendReset(ctx, providerName, accountID, &resetSpendRequest{optionID: optionID, revision: revision})
}

// FinishProviderAccountReset sends the account's unsettled spend spendID
// again under its key and credit, which never takes a second credit, then
// reads the account's usage again. It is refused unsent when that spend is
// not the account's unsettled one.
func (m *Manager) FinishProviderAccountReset(
	ctx context.Context,
	providerName, accountID, spendID string,
) (ResetSpendResult, error) {
	return m.spendReset(ctx, providerName, accountID, &resetSpendRequest{finish: true, spendID: spendID})
}

func (m *Manager) spendReset(
	ctx context.Context,
	providerName, accountID string,
	spend *resetSpendRequest,
) (ResetSpendResult, error) {
	if providerName != string(provider.Codex) {
		return ResetSpendResult{}, errorsx.Public("reset_unsupported",
			"This account has no usage resets to use here.", fmt.Errorf("spend a %s usage reset", providerName))
	}
	spend.sendBy = time.Now().Add(m.resetSendCutoff())
	err := m.usageTransaction(ctx, providerName, accountID, spend)
	if spend.outcome == codex.ResetOutcomeReset && spend.result.ReadingLanded && m.deps.Accounts != nil {
		m.deps.Accounts.PublishLimitsReset(providerName, accountID)
	}
	if spend.result.Message != "" && err != nil {
		// Codex settled the spend, so its answer is what the person needs,
		// and what failed after it is logged whole. A failed reading is
		// said beside the answer: the card does not show the account as it
		// is. A failed settle save alone needs nothing from the person: it
		// leaves the spend unsettled, and Finish sends its key again.
		log.Printf("provideraccountapp: after the %s reset for %s: %v", providerName, accountID, err)
		if !spend.result.ReadingLanded {
			spend.result.Message += " Reading the account's usage after it failed, so refresh the account to see its limits."
		}
		return spend.result, nil
	}
	if err != nil && spend.pending == nil {
		// Nothing was recorded, so nothing was sent; whatever stopped the
		// spend, that is what the person reads.
		if _, _, public := errorsx.PublicDetails(err); !public {
			err = errorsx.Public("reset_unsent", unsentSentence(spend.finish), err)
		}
	}
	return spend.result, err
}

func (m *Manager) resetSendCutoff() time.Duration {
	if m.resetSendCutoffOverride > 0 {
		return m.resetSendCutoffOverride
	}
	return ResetSendCutoff
}

// lockForUsage takes the provider's reconcile lock for a usage reading, and
// for a spend waits at most resetLockWait for it.
func (m *Manager) lockForUsage(ctx context.Context, mu *reconcileLock, providerName, accountID string, spend *resetSpendRequest) error {
	if spend == nil {
		if err := mu.LockContext(ctx); err != nil {
			return fmt.Errorf("wait for the %s account lock to read account %s's usage: %w", providerName, accountID, err)
		}
		return nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, m.resetLockWait())
	defer cancel()
	if err := mu.LockContext(lockCtx); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("wait for the %s account lock to spend account %s's reset: %w", providerName, accountID, err)
		}
		return errorsx.Public("reset_busy", "Another account action was still running, so "+
			unsentTail(spend.finish), nil)
	}
	return nil
}

// resetSelectionMovedError refuses a spend of the selected account after the
// reconciliation at its start found another login in the canonical store and
// selected it: the person chose the reset for an account that is no longer
// the one Codex runs as. Caller holds the reconcile lock and no Manager lock.
func (m *Manager) resetSelectionMovedError(providerName string, spend *resetSpendRequest) error {
	m.mu.RLock()
	active, ok := m.store.Active(providerName, time.Now())
	m.mu.RUnlock()
	signedIn := "another account"
	if ok && strings.TrimSpace(active.Email) != "" {
		signedIn = DescribeObservedAccount(AccountInfo(active))
	}
	return errorsx.Public("reset_account_switched", fmt.Sprintf(
		"Codex was signed in to %s outside the app, so the app switched to that account and %s",
		signedIn, unsentTail(spend.finish)), nil)
}

// unsentTail ends a sentence about a request that was never sent: a fresh
// spend used nothing, but the request a Finish follows may have.
func unsentTail(finish bool) string {
	if finish {
		return "Finish wasn't sent. The first request may still have used the reset, so press Finish again later."
	}
	return "the reset wasn't sent. No reset was used. Try again."
}

// unsentSentence is unsentTail as a sentence of its own.
func unsentSentence(finish bool) string {
	if finish {
		return unsentTail(true)
	}
	return "The reset wasn't sent. No reset was used. Try again."
}

// checkResetSpend refuses, before anything is started, a spend that could
// not be admitted: for a fresh spend the store's rule (CheckResetSpend), for
// Finish an unsettled spend other than the one the card showed. Caller holds
// the reconcile lock, so the answer holds until admitResetSpend.
func (m *Manager) checkResetSpend(providerName, accountID string, spend *resetSpendRequest) error {
	if spend.finish {
		_, err := m.unsettledSpend(providerName, accountID, spend.spendID)
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return resetBeginError(m.store.CheckResetSpend(providerName, accountID, spend.revision, spend.optionID))
}

// unsettledSpend is the account's unsettled spend when it is spendID, the
// one the card showed; Finish sends nothing else.
func (m *Manager) unsettledSpend(providerName, accountID, spendID string) (provideraccounts.PendingReset, error) {
	m.mu.RLock()
	pending, ok := m.store.PendingReset(providerName, accountID)
	m.mu.RUnlock()
	switch {
	case !ok:
		return pending, errResetNothingUnsettled
	case pending.ID != spendID:
		return pending, errResetFinishOther
	}
	return pending, nil
}

// resetBeginError is the sentence for a refusal of the store's spend rule.
func resetBeginError(err error) error {
	var unsettled *provideraccounts.ResetPendingError
	var unusable *provideraccounts.ResetUnusableError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unsettled):
		return errorsx.Public("reset_unsettled", fmt.Sprintf(
			"A %s on this account isn't confirmed yet, so no other reset was sent. Press Finish on the account to complete it first.",
			unsettled.Pending.Label), err)
	case errors.Is(err, provideraccounts.ErrResetListStale):
		return ErrResetListChanged
	case errors.As(err, &unusable):
		return errorsx.Public("reset_unusable",
			fmt.Sprintf("This %s can't be used now. %s", unusable.Option.Label, unusable.Option.Reason), err)
	default:
		return fmt.Errorf("record the reset before using it: %w", err)
	}
}

// admitResetSpend is the spend's last step before its consume is written
// (codex.ProbeConfig.BeforeConsume): it confirms the login Codex reported is
// the chosen account and records a fresh spend durably. It returns what the
// consume carries; an error sends nothing. changed reports a write to the
// account's listing that nothing published yet; the transaction publishes it
// once the consume was written, and the probe's last check before that write
// is the cutoff (codex.ErrResetLate). Caller holds the reconcile lock and no
// Manager lock.
func (m *Manager) admitResetSpend(
	providerName string,
	account provideraccounts.Account,
	observed provider.AccountInfo,
	credential []byte,
	spend *resetSpendRequest,
) (send codex.ResetSpend, changed bool, err error) {
	if err := m.confirmResetAccount(account, observed, credential, spend.finish); err != nil {
		return codex.ResetSpend{}, false, err
	}
	var pending provideraccounts.PendingReset
	if spend.finish {
		if pending, err = m.unsettledSpend(providerName, account.ID, spend.spendID); err != nil {
			return codex.ResetSpend{}, false, err
		}
	} else {
		m.mu.Lock()
		pending, err = m.store.BeginResetSpend(providerName, account.ID, spend.revision, spend.optionID,
			provideraccounts.PendingReset{ID: uuid.NewString(), Key: uuid.NewString(), StartedAt: time.Now().UnixMilli()})
		m.mu.Unlock()
		if err != nil {
			return codex.ResetSpend{}, false, resetBeginError(err)
		}
	}
	spend.pending = &pending
	if m.resetAdmitted != nil {
		m.resetAdmitted()
	}
	return codex.ResetSpend{IdempotencyKey: pending.Key, CreditID: pending.CreditID}, !spend.finish, nil
}

// confirmResetAccount accepts the login a spend's app-server reported only
// when it proves to be the saved account the person chose
// (provideraccounts.Identity.Proves): the same email on the same known
// workspace. Codex's account/read names no workspace, so it is the one the
// credential carries; a workspace unknown on either side could be another
// saved account of that email, and is refused. Anything else sends nothing.
func (m *Manager) confirmResetAccount(account provideraccounts.Account, observed provider.AccountInfo, credential []byte, finish bool) error {
	observed = EnrichCodexIdentity(string(provider.Codex), observed, credential)
	if strings.TrimSpace(observed.Email) == "" {
		return errorsx.Public("reset_account_unknown",
			"Codex did not say which account is signed in, so "+unsentTail(finish), nil)
	}
	m.mu.RLock()
	saved, ok := m.store.Get(string(provider.Codex), account.ID, time.Now())
	m.mu.RUnlock()
	identity := provideraccounts.IdentityFromInfo(observed)
	switch {
	case ok && provideraccounts.AccountIdentity(saved).Proves(identity):
		return nil
	case ok && provideraccounts.AccountIdentity(saved).Confirms(identity):
		sentence := "Codex is signed in as %s, but which workspace this account is on isn't known yet, " +
			"so the reset wasn't sent. No reset was used. Refresh the account, then try again."
		if finish {
			sentence = "Codex is signed in as %s, but which workspace this account is on isn't known yet, " +
				"so Finish wasn't sent. The first request may still have used the reset. Refresh the account, then press Finish again."
		}
		return errorsx.Public("reset_account_unproven", fmt.Sprintf(sentence, DescribeObservedAccount(observed)), nil)
	default:
		return errorsx.Public("reset_account_mismatch", fmt.Sprintf(
			"Codex is signed in as %s, not %s, so %s Refresh the account first.",
			DescribeObservedAccount(observed), account.Email, unsentTail(finish)), nil)
	}
}

// settleAnsweredResetSpend settles the spend on receipt of Codex's answer
// (codex.ProbeConfig.OnConsumeAnswer): the result the person reads, and the
// record ended in one save that drops the credit when the answer says it is
// gone. changed reports a write to the account's listing. Caller holds the
// reconcile lock and no Manager lock.
func (m *Manager) settleAnsweredResetSpend(accountID, outcome string, spend *resetSpendRequest) (changed bool, err error) {
	spend.outcome = outcome
	spend.result.Message, spend.result.Spent = codexResetMessage(outcome, spend.finish)
	m.mu.Lock()
	settled, err := m.store.SettleResetSpend(string(provider.Codex), accountID, spend.pending.Key, spend.goneCreditID() != "")
	m.mu.Unlock()
	if err != nil {
		return false, fmt.Errorf("settle the Codex reset: %w", err)
	}
	return settled, nil
}

// settleUnansweredResetSpend turns SpendReset's error, when Codex gave no
// answer, into the error the person sees, and ends the unsettled spend when
// that error says nothing was used: a fresh spend's consume was refused,
// never written, or past its cutoff. changed reports a write to the
// account's listing. Caller holds the reconcile lock and no Manager lock.
func (m *Manager) settleUnansweredResetSpend(accountID string, spendErr error, spend *resetSpendRequest) (changed bool, err error) {
	if spend.pending == nil {
		// Nothing was recorded, so nothing was sent (spendReset words it).
		return false, spendErr
	}
	var unconfirmed *codex.ResetUnconfirmedError
	var refused *codex.ResetRefusedError
	switch {
	case errors.As(spendErr, &unconfirmed):
		log.Printf("provideraccountapp: codex reset for %s unconfirmed: %v", accountID, spendErr)
		return false, errResetUnconfirmed
	case errors.Is(spendErr, codex.ErrResetLate):
		changed, keptErr := m.endUnsent(accountID, spend)
		if keptErr != nil {
			// The record stays, and the card offers Finish for it.
			return changed, errorsx.Public("reset_late_kept",
				"Codex took too long to start, so the reset wasn't sent. The account still shows it as unconfirmed, "+
					"and pressing Finish there would send it.", keptErr)
		}
		return changed, errorsx.Public("reset_late", "Codex took too long to start, so "+unsentTail(spend.finish), spendErr)
	case errors.As(spendErr, &refused):
		changed, keptErr := m.endUnsent(accountID, spend)
		return changed, errors.Join(codexRefusalError(refused, spend.finish), keptErr)
	default:
		// Admitted but never written: SpendReset reports every failure after
		// the write as unconfirmed.
		if _, _, public := errorsx.PublicDetails(spendErr); !public {
			spendErr = errorsx.Public("reset_unsent", unsentSentence(spend.finish), spendErr)
		}
		changed, keptErr := m.endUnsent(accountID, spend)
		return changed, errors.Join(spendErr, keptErr)
	}
}

// endUnsent ends a fresh spend whose consume Codex refused or never got: it
// used nothing. A Finish keeps the spend, since the first request under its
// key may still have landed. An error is the failed save, which leaves the
// record standing.
func (m *Manager) endUnsent(accountID string, spend *resetSpendRequest) (bool, error) {
	if spend.finish {
		return false, nil
	}
	m.mu.Lock()
	settled, err := m.store.SettleResetSpend(string(provider.Codex), accountID, spend.pending.Key, false)
	m.mu.Unlock()
	if err != nil {
		return false, fmt.Errorf("end the unsent Codex reset: %w", err)
	}
	return settled, nil
}

func codexResetMessage(outcome string, finish bool) (message string, spent bool) {
	switch outcome {
	case codex.ResetOutcomeReset:
		return "Reset used. Your Codex usage limits are refilled.", true
	case codex.ResetOutcomeAlreadyRedeemed:
		return "This reset was already used, and your Codex usage limits were refilled then.", true
	case codex.ResetOutcomeNothingToReset:
		return "This account isn't at a usage limit, so there was nothing to reset. The reset wasn't used.", false
	default: // codex.ResetOutcomeNoCredit
		if finish {
			return "This reset is no longer available: the first request used it, or it expired.", false
		}
		return "This account has no reset left to use.", false
	}
}

func codexRefusalError(refused *codex.ResetRefusedError, finish bool) error {
	if finish {
		return errorsx.Public("reset_refused", fmt.Sprintf("Codex refused Finish (%s). The first request may "+
			"still have used the reset, so press Finish again later.", refused.Message), refused)
	}
	if refused.MethodMissing() {
		return errResetCodexTooOld
	}
	return errorsx.Public("reset_refused",
		fmt.Sprintf("Codex refused the reset (%s). No reset was used.", refused.Message), refused)
}

// Codex reset wording when a credit carries none of its own.
const (
	codexResetLabel   = "Full reset"
	codexResetRefills = "Refills your current Codex usage limits."
)

// codexResetReading maps a Codex reading's credits onto the card's list: one
// option per listed credit that is available or being applied,
// earliest-expiring first, and the available credits the backend counted
// beyond its capped list, or with no list at all, as Unlisted
// (codex-wire.md#rate-limit-reset-credits: availableCount is authoritative,
// the list may be capped). gone is a credit a spend's answer says is used or
// gone: a reading that still lists it is stale about it, so it is neither
// offered nor counted. note describes a summary the card cannot show as Codex
// gave it; it is empty otherwise.
func codexResetReading(credits *codex.ResetCredits, gone string) (reading provideraccounts.ResetReading, note string) {
	reading.Options = []provideraccounts.UsageResetOption{}
	available := credits.AvailableCount
	listed := 0
	for _, credit := range credits.Credits {
		if gone != "" && credit.ID == gone {
			if credit.Status == codex.ResetCreditAvailable && available > 0 {
				available--
			}
			continue
		}
		option := provideraccounts.UsageResetOption{
			ID:        credit.ID,
			Kind:      credit.ResetType,
			Label:     credit.Title,
			Refills:   credit.Description,
			ExpiresAt: credit.ExpiresAt,
		}
		if option.Label == "" {
			option.Label = codexResetLabel
		}
		if option.Refills == "" {
			option.Refills = codexResetRefills
		}
		switch credit.Status {
		case codex.ResetCreditAvailable:
			option.Usable = true
			listed++
		case codex.ResetCreditRedeeming:
			option.Reason = "Codex is still applying this reset."
		default:
			continue
		}
		reading.Options = append(reading.Options, option)
	}
	slices.SortStableFunc(reading.Options, func(a, b provideraccounts.UsageResetOption) int {
		return compareExpiry(a.ExpiresAt, b.ExpiresAt)
	})
	switch {
	case available > listed:
		reading.Unlisted = available - listed
	case available < listed:
		note = fmt.Sprintf("Codex counted %d available resets and listed %d", available, listed)
	}
	return reading, note
}

// codexResetReadingLocked is codexResetReading, logging its note once per
// change for the account. Caller holds mu.
func (m *Manager) codexResetReadingLocked(accountID string, credits *codex.ResetCredits, gone string) provideraccounts.ResetReading {
	reading, note := codexResetReading(credits, gone)
	if m.resetNotes[accountID] != note {
		if note != "" {
			log.Printf("provideraccountapp: codex account %s: %s", accountID, note)
		}
		if m.resetNotes == nil {
			m.resetNotes = map[string]string{}
		}
		m.resetNotes[accountID] = note
	}
	return reading
}

// compareExpiry orders expiry times; 0 (never expires) is last.
func compareExpiry(a, b int64) int {
	switch {
	case a == b:
		return 0
	case a == 0:
		return 1
	case b == 0:
		return -1
	case a < b:
		return -1
	default:
		return 1
	}
}

// recordCodexUsageResetsLocked writes the resets a Codex reading named onto
// the account row, without gone (codexResetReading). The caller has proven
// the reading's credential is the account's. credits nil is a reading that
// said nothing about resets (an older Codex, an API-key login, a failed
// lookup): the stored list stays as it is. A reading the store refuses as
// invalid is not recorded, and the next valid one replaces the list. Caller
// holds mu and the reconcile lock.
func (m *Manager) recordCodexUsageResetsLocked(accountID string, credits *codex.ResetCredits, gone string) (bool, error) {
	if credits == nil {
		return false, nil
	}
	_, changed, err := m.store.SetUsageResets(string(provider.Codex), accountID, m.codexResetReadingLocked(accountID, credits, gone))
	if err != nil {
		return false, fmt.Errorf("record the Codex account's usage resets: %w", err)
	}
	return changed, nil
}
