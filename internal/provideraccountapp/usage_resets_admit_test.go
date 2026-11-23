package provideraccountapp

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
)

// watchListings fails the test when a listing is published while the
// account has an unsettled spend that wantPending says it should not have.
func watchListings(t *testing.T, rig resetRig, wantPending bool) *int {
	t.Helper()
	published := 0
	rig.sink.onListing = func() {
		published++
		if _, ok := rig.store.PendingReset(codexName, "inactive"); ok != wantPending {
			t.Errorf("a listing was published with an unsettled spend present=%v, want %v", ok, wantPending)
		}
	}
	return &published
}

// The record exists only once the consume is about to be written: an
// app-server that ends before saying whose login it runs leaves no record at
// any moment, publishes nothing, and the person reads that nothing was used.
func TestAnExitBeforeTheConsumeLeavesNoRecordAtAnyMoment(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.write(t, "exit-at-account", "")
	published := watchListings(t, rig, false)
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if publicCode(err) != "reset_unsent" || !strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("err = %v, want the unsent sentence", err)
	}
	if *published != 0 {
		t.Fatalf("a spend that never reached its consume published %d listings", *published)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a spend that never reached its consume left a record")
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("consumes = %v", consumes)
	}
	if got := rig.resets(t); got.Revision != resets.Revision {
		t.Fatalf("a spend that never reached its consume moved the revision to %d", got.Revision)
	}
}

// Finish names the spend the card showed; another unsettled spend is refused
// unsent.
func TestFinishOfAnotherSpendIsRefusedUnsent(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsumeFrame(t, "")
	_, _ = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	pending, ok := rig.pending(t)
	if !ok {
		t.Fatal("the lost answer left no record")
	}
	rig.answerConsume(t, "alreadyRedeemed")
	if _, err := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", "a-spend-the-card-showed-earlier"); publicCode(err) != "reset_finish_changed" {
		t.Fatalf("Finish of another spend: err = %v", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 1 {
		t.Fatalf("a Finish of another spend was sent: %v", consumes)
	}
	if got, ok := rig.pending(t); !ok || got != pending {
		t.Fatalf("a refused Finish changed the record to %+v (ok=%v)", got, ok)
	}
}

// stallPastCutoff makes a spend's admission, after its record is written
// (the last step before the probe's cutoff check), run past the cutoff;
// atStall runs first.
func stallPastCutoff(rig resetRig, atStall func()) {
	cutoff := 3 * time.Second
	rig.manager.resetSendCutoffOverride = cutoff
	start := time.Now()
	rig.manager.resetAdmitted = func() {
		if atStall != nil {
			atStall()
		}
		time.Sleep(time.Until(start.Add(cutoff)) + 50*time.Millisecond)
	}
}

// A spend whose steps before the consume ran past its cutoff sends nothing;
// the record it made is deleted before any card could offer Finish for it.
func TestASpendPastItsCutoffIsNotSent(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	stallPastCutoff(rig, nil)
	published := watchListings(t, rig, false)
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if publicCode(err) != "reset_late" || !strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("err = %v, want the late refusal", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend past its cutoff was sent: %v", consumes)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a spend past its cutoff kept its record")
	}
	// The record's save moved the revision, so the cards re-read the list.
	if *published != 1 {
		t.Fatalf("listings published = %d, want one after the record was deleted", *published)
	}
}

// When the record of a spend past its cutoff cannot be deleted, nothing is
// sent, the card offers Finish for it, and the person is told Finish would
// send it.
func TestASpendPastItsCutoffWhoseRecordCannotBeDeletedSaysFinishWouldSendIt(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	t.Cleanup(func() { _ = os.Chmod(rig.storeDir, 0o700) })
	stallPastCutoff(rig, func() {
		if err := os.Chmod(rig.storeDir, 0o500); err != nil {
			t.Error(err)
		}
	})
	published := watchListings(t, rig, true)
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if publicCode(err) != "reset_late_kept" || !strings.Contains(err.Error(), "pressing Finish there would send it") {
		t.Fatalf("err = %v, want the kept-record refusal", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend past its cutoff was sent: %v", consumes)
	}
	if _, ok := rig.pending(t); !ok {
		t.Fatal("the record that could not be deleted is gone")
	}
	if *published != 1 {
		t.Fatalf("listings published = %d, want one so the card offers Finish", *published)
	}
}

// With no reading after a settled answer, an answer that says the credit is
// gone removes it by id in the settle; nothingToReset leaves it.
func TestASettledSpendWithoutAReadingDropsOnlyACreditTheAnswerSaysIsGone(t *testing.T) {
	for outcome, dropped := range map[string]bool{
		"reset": true, "alreadyRedeemed": true, "noCredit": true, "nothingToReset": false,
	} {
		t.Run(outcome, func(t *testing.T) {
			rig := newResetRig(t, nil)
			resets := seedResetAccount(t, rig, twoCredits)
			rig.answerConsume(t, outcome)
			rig.write(t, "ratelimits.json", "")
			result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
			if err != nil || result.ReadingLanded {
				t.Fatalf("result = %+v, %v; want the answer without a reading", result, err)
			}
			kept := false
			for _, option := range rig.resets(t).Options {
				kept = kept || option.ID == "RateLimitResetCredit_soon"
			}
			if kept == dropped {
				t.Fatalf("after %s the list is %+v", outcome, rig.resets(t).Options)
			}
		})
	}
}

// A spend goes out only when the login proves to be the chosen account: two
// saved accounts sharing the email, and a login that names no workspace, is
// refused with nothing sent.
func TestASpendUnderALoginTwoSavedAccountsCouldBeSendsNothing(t *testing.T) {
	rig := newResetRig(t, nil)
	for _, account := range []provideraccounts.Account{
		{ID: "inactive", Provider: codexName, Email: "shared@example.com", OrgID: "ws-a", SubscriptionType: "team"},
		{ID: "twin", Provider: codexName, Email: "shared@example.com", OrgID: "ws-b", SubscriptionType: "team"},
	} {
		if _, err := rig.store.Add(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.creds.WriteAccountCredential(codexName, "inactive",
		[]byte(`{"tokens":{"access_token":"inactive","refresh_token":"inactive-refresh"}}`)); err != nil {
		t.Fatal(err)
	}
	rig.write(t, "account.json", chatgptAccountFrame("shared@example.com", "team", "")+"\n")
	rig.holdResets(t, twoCredits)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatalf("first reading: %v", err)
	}
	resets := rig.resets(t)
	if resets == nil {
		t.Fatal("the first reading recorded no list")
	}
	rig.answerConsume(t, "reset")
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if publicCode(err) != "reset_account_unproven" || !strings.Contains(err.Error(), "No reset was used") ||
		!strings.Contains(err.Error(), "Refresh the account") {
		t.Fatalf("err = %v, want the unproven-login refusal", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend went out under an ambiguous login: %v", consumes)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a refused spend left a record")
	}
}

// A lone account of an email is spent on only when its workspace and the
// login's are both known and equal: a workspace unknown on either side is
// refused unsent, and says to refresh the account. Finish says so too.
func TestASpendWithAWorkspaceUnknownOnEitherSideSendsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		orgID      string
		credential string
	}{
		"saved row": {"", inactiveCredential},
		"login":     {"ws-inactive", `{"tokens":{"access_token":"inactive","refresh_token":"inactive-refresh"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newResetRig(t, nil)
			if _, err := rig.store.Add(provideraccounts.Account{ID: "inactive", Provider: codexName,
				Email: "inactive@example.com", OrgID: tc.orgID, SubscriptionType: "team"}); err != nil {
				t.Fatal(err)
			}
			if err := rig.creds.WriteAccountCredential(codexName, "inactive", []byte(tc.credential)); err != nil {
				t.Fatal(err)
			}
			resets, _, err := rig.store.SetUsageResets(codexName, "inactive", provideraccounts.ResetReading{
				Options: []provideraccounts.UsageResetOption{{ID: "RateLimitResetCredit_soon", Kind: "codexRateLimits", Label: "Full reset", Usable: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			rig.answerConsume(t, "reset")
			_, err = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.UsageResets.Revision)
			if publicCode(err) != "reset_account_unproven" || !strings.Contains(err.Error(), "No reset was used") ||
				!strings.Contains(err.Error(), "Refresh the account") {
				t.Fatalf("err = %v, want the unproven-login refusal", err)
			}
			if consumes := rig.consumes(t); len(consumes) != 0 {
				t.Fatalf("a spend went out under an unproven login: %v", consumes)
			}
			if _, ok := rig.pending(t); ok {
				t.Fatal("a refused spend left a record")
			}
		})
	}
}

// selectedRig saves "picked" as the selected Codex account, with the same
// credential in its slot and in the canonical store.
func selectedRig(t *testing.T) (resetRig, []byte) {
	t.Helper()
	rig := newResetRig(t, nil)
	if _, err := rig.store.Add(provideraccounts.Account{ID: "picked", Provider: codexName, Email: "picked@example.com", OrgID: "ws-picked", SubscriptionType: "team"}); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"tokens":{"access_token":"picked","refresh_token":"picked-refresh","account_id":"ws-picked"},"last_refresh":"2029-01-01T00:00:00Z"}`)
	if err := rig.creds.WriteAccountCredential(codexName, "picked", seed); err != nil {
		t.Fatal(err)
	}
	if err := rig.creds.WriteNativeCredentialForTest(codexName, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Activate(codexName, "picked"); err != nil {
		t.Fatal(err)
	}
	return rig, seed
}

func (r resetRig) credential(t *testing.T, accountID string, active bool) string {
	t.Helper()
	snapshot, err := r.creds.ReadCredentialSnapshot(codexName, accountID, active)
	if err != nil {
		t.Fatal(err)
	}
	return string(snapshot.Bytes())
}

func (r resetRig) resetsOf(t *testing.T, accountID string) *provideraccounts.UsageResets {
	t.Helper()
	account, ok := r.store.Get(codexName, accountID, time.Now())
	if !ok {
		t.Fatalf("account %s missing", accountID)
	}
	return account.UsageResets
}

// A selected account's probe that does not prove whose login the canonical
// credential is writes nothing: the canonical store and the slot keep what
// they held, and its reset list is not recorded.
func TestASelectedProbeThatNamesNoLoginWritesNothingOfTheAccounts(t *testing.T) {
	rig, seed := selectedRig(t)
	rotated := `{"tokens":{"access_token":"rotated","refresh_token":"rotated-refresh","account_id":"ws-picked"},"last_refresh":"2030-01-01T00:00:00Z"}`
	rig.write(t, "rotate.json", rotated)
	rig.write(t, "account.json", `{"jsonrpc":"2.0","id":3,"result":{"account":null,"requiresOpenaiAuth":true}}`+"\n")
	rig.holdResets(t, twoCredits)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "picked"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := rig.credential(t, "", true); got != string(seed) {
		t.Fatalf("canonical = %s, want it untouched", got)
	}
	if got := rig.credential(t, "picked", false); got != string(seed) {
		t.Fatalf("slot = %s, want it untouched", got)
	}
	if got := rig.resetsOf(t, "picked"); got != nil {
		t.Fatalf("an unproven reading recorded resets %+v", got)
	}
}

// A selected account's probe whose reading failed still says whose login it
// ran: another saved account's rotation reaches neither this account's slot
// nor the canonical store.
func TestAFailedSelectedProbeOfAnotherLoginKeepsItsRotationOutOfTheSlot(t *testing.T) {
	rig, seed := selectedRig(t)
	if _, err := rig.store.Add(provideraccounts.Account{ID: "other", Provider: codexName, Email: "other@example.com", SubscriptionType: "team"}); err != nil {
		t.Fatal(err)
	}
	rotated := `{"tokens":{"access_token":"rotated","refresh_token":"rotated-refresh","account_id":"ws-picked"},"last_refresh":"2030-01-01T00:00:00Z"}`
	rig.write(t, "rotate.json", rotated)
	rig.write(t, "account.json", chatgptAccountFrame("other@example.com", "team", "")+"\n")
	rig.write(t, "ratelimits.json", "")
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "picked"); err == nil {
		t.Fatal("a failed reading reported no error")
	}
	if got := rig.credential(t, "picked", false); got != string(seed) {
		t.Fatalf("slot = %s, want another login's rotation kept out", got)
	}
	if got := rig.credential(t, "", true); got != string(seed) {
		t.Fatalf("canonical = %s, want it untouched", got)
	}
}

// A selected account's probe whose reading failed after account/read named
// the account's own login proves the credential: its rotation reaches the
// slot as well as the canonical store.
func TestAFailedSelectedProbeOfItsOwnLoginLandsItsRotationInTheSlot(t *testing.T) {
	rig, _ := selectedRig(t)
	rotated := `{"tokens":{"access_token":"rotated","refresh_token":"rotated-refresh","account_id":"ws-picked"},"last_refresh":"2030-01-01T00:00:00Z"}`
	rig.write(t, "rotate.json", rotated)
	rig.write(t, "account.json", chatgptAccountFrame("picked@example.com", "team", "")+"\n")
	rig.write(t, "ratelimits.json", "")
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "picked"); err == nil {
		t.Fatal("a failed reading reported no error")
	}
	if got := rig.credential(t, "picked", false); got != rotated {
		t.Fatalf("slot = %s, want the rotation its own login made", got)
	}
	if got := rig.credential(t, "", true); got != rotated {
		t.Fatalf("canonical = %s, want the rotation", got)
	}
}

// An identity probe that adopts an account it reported a login for records
// that reading's resets on it, in the adopting step; one that reported no
// login records nothing.
func TestAnAdoptingProbeRecordsItsResetsOnTheAccount(t *testing.T) {
	credits := &codex.ResetCredits{AvailableCount: 1, Credits: []codex.ResetCredit{
		{ID: "RateLimitResetCredit_1", ResetType: "codexRateLimits", Status: codex.ResetCreditAvailable},
	}}
	for name, email := range map[string]string{"login reported": "picked@example.com", "no login": ""} {
		t.Run(name, func(t *testing.T) {
			rig, _ := selectedRig(t)
			_, err := rig.manager.RunAccountProbe(ProbeRequest{
				ProviderName: codexName,
				Probe: func(context.Context) (provider.AccountInfo, error) {
					return provider.AccountInfo{Email: email, SubscriptionType: "team"}, nil
				},
				ResetCredits: func() (*codex.ResetCredits, bool) { return credits, true },
			})
			if err != nil {
				t.Fatal(err)
			}
			got := rig.resetsOf(t, "picked")
			if recorded := got != nil && len(got.Options) == 1; recorded != (email != "") {
				t.Fatalf("resets after the probe = %+v", got)
			}
			if published := len(rig.sink.listings) == 1; published != (email != "") {
				t.Fatalf("listings published = %v", rig.sink.listings)
			}
		})
	}
}

// The outside sign-in sequence: A and B are saved under one email on two
// workspaces, A is selected, someone signs in to B outside the app, and A's
// usage is refreshed before reconciliation sees it. The probe runs B's login.
// Whatever it reads (a good reading, a rotation with a failed reading, or
// resets without usage windows), A's slot and A's list stay as they were.
// With B's workspace on its credential the reading is B's; without it nothing
// proves the reading is A's, so a reading that is not whole lands nothing.
func TestARefreshUnderAnOutsideLoginOfAnotherWorkspaceLeavesTheAccountAlone(t *testing.T) {
	const bWithWorkspace = `{"tokens":{"access_token":"b","refresh_token":"b-refresh","account_id":"ws-b"},"last_refresh":"2029-01-01T00:00:00Z"}`
	const bWithoutWorkspace = `{"tokens":{"access_token":"b","refresh_token":"b-refresh"},"last_refresh":"2029-01-01T00:00:00Z"}`
	readings := map[string]func(rig resetRig){
		"good reading": func(rig resetRig) { rig.holdResets(t, lateCreditOnly) },
		"rotation and a failed reading": func(rig resetRig) {
			rig.write(t, "rotate.json", `{"tokens":{"access_token":"b2","refresh_token":"b2-refresh","account_id":"ws-b"},"last_refresh":"2030-01-01T00:00:00Z"}`)
			rig.write(t, "ratelimits.json", "")
		},
		"resets without usage windows": func(rig resetRig) {
			rig.write(t, "ratelimits.json", `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team"},"rateLimitResetCredits":`+lateCreditOnly+`}}`+"\n")
		},
	}
	for _, credential := range []string{bWithWorkspace, bWithoutWorkspace} {
		for name, read := range readings {
			// A whole reading under a login that names no workspace is
			// attributed by email, as every reading before this one was.
			if credential == bWithoutWorkspace && name == "good reading" {
				continue
			}
			t.Run(name+" workspace="+strconv.FormatBool(credential == bWithWorkspace), func(t *testing.T) {
				rig := newResetRig(t, nil)
				for _, account := range []provideraccounts.Account{
					{ID: "a", Provider: codexName, Email: "shared@example.com", OrgID: "ws-a", SubscriptionType: "team"},
					{ID: "b", Provider: codexName, Email: "shared@example.com", OrgID: "ws-b", SubscriptionType: "team"},
				} {
					if _, err := rig.store.Add(account); err != nil {
						t.Fatal(err)
					}
				}
				aSeed := `{"tokens":{"access_token":"a","refresh_token":"a-refresh","account_id":"ws-a"},"last_refresh":"2029-01-01T00:00:00Z"}`
				if err := rig.creds.WriteAccountCredential(codexName, "a", []byte(aSeed)); err != nil {
					t.Fatal(err)
				}
				if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(aSeed)); err != nil {
					t.Fatal(err)
				}
				if _, err := rig.store.Activate(codexName, "a"); err != nil {
					t.Fatal(err)
				}
				before, _, err := rig.store.SetUsageResets(codexName, "a", provideraccounts.ResetReading{
					Options: []provideraccounts.UsageResetOption{{ID: "RateLimitResetCredit_a", Kind: "codexRateLimits", Label: "Full reset", Usable: true}},
				})
				if err != nil {
					t.Fatal(err)
				}
				// The outside sign-in to B.
				if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(credential)); err != nil {
					t.Fatal(err)
				}
				rig.write(t, "account.json", chatgptAccountFrame("shared@example.com", "team", "")+"\n")
				read(rig)
				err = rig.manager.RefreshProviderAccountUsage(codexName, "a")
				if got := rig.credential(t, "a", false); got != aSeed {
					t.Fatalf("A's slot = %s (refresh err %v), want it as it was", got, err)
				}
				after := rig.resetsOf(t, "a")
				if after == nil || after.Revision != before.UsageResets.Revision || len(after.Options) != 1 || after.Options[0].ID != "RateLimitResetCredit_a" {
					t.Fatalf("A's list = %+v (refresh err %v), want it as it was", after, err)
				}
				if got := rig.credential(t, "", true); got != credential {
					t.Fatalf("canonical = %s, want B's login as it was", got)
				}
			})
		}
	}
}

// A fresh spend's record is published once its consume is out, before Codex
// answers it, so another client offers Finish instead of a Use meanwhile.
func TestASpendsRecordIsPublishedWhileItsRequestIsOut(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	var pendingAtPublish []bool
	rig.sink.onListing = func() {
		_, ok := rig.store.PendingReset(codexName, "inactive")
		pendingAtPublish = append(pendingAtPublish, ok)
	}
	if _, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision); err != nil {
		t.Fatal(err)
	}
	if len(pendingAtPublish) != 2 || !pendingAtPublish[0] || pendingAtPublish[1] {
		t.Fatalf("listings published with the record present = %v, want it once while out, then once settled", pendingAtPublish)
	}
}
