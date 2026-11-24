package provideraccountapp

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

const codexName = string(provider.Codex)

// resetRig is a Codex app-server script whose answers are files a test
// rewrites between calls: the account/read answer (with exit-at-account
// present the process exits instead of answering), the consume answer
// (empty: the process exits without answering), the rate-limit answer
// (empty: the process exits without answering), and a credential it writes
// into its home as a rotation when the rate-limit read arrives. Every
// consume it is sent is logged; with freeze-store present it then makes the
// store's directory read-only, which is a crash between Codex's answer and
// the save that settles it. Every rate-limit read copies the store's file to
// store-at-reading.json, so a test sees what was saved before the reading.
type resetRig struct {
	dir      string
	storeDir string
	manager  *Manager
	store    *provideraccounts.Store
	creds    *provideraccounts.Credentials
	sink     *recordingAccountSink
}

func (r resetRig) file(name string) string { return filepath.Join(r.dir, name) }

func (r resetRig) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(r.file(name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r resetRig) answerConsume(t *testing.T, outcome string) {
	t.Helper()
	r.write(t, "consume.json", `{"jsonrpc":"2.0","id":4,"result":{"outcome":"`+outcome+`"}}`+"\n")
}

func (r resetRig) answerConsumeFrame(t *testing.T, frame string) {
	t.Helper()
	r.write(t, "consume.json", frame)
}

// holdResets sets the reset summary the next readings carry.
func (r resetRig) holdResets(t *testing.T, summary string) {
	t.Helper()
	r.write(t, "ratelimits.json", `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team",`+
		`"primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":4102444800}},"rateLimitResetCredits":`+summary+`}}`+"\n")
}

// consumes is every consume request sent so far, as its params.
func (r resetRig) consumes(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(r.file("consumes.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var frame struct {
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("consume line %q: %v", line, err)
		}
		out = append(out, frame.Params)
	}
	return out
}

// pending is the account's unsettled spend.
func (r resetRig) pending(t *testing.T) (provideraccounts.PendingReset, bool) {
	t.Helper()
	return r.store.PendingReset(codexName, "inactive")
}

// listed is the account as the card's listing carries it.
func (r resetRig) listed(t *testing.T) ManagedAccount {
	t.Helper()
	accounts, err := r.manager.ListProviderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.Provider == codexName && account.ID == "inactive" {
			return account
		}
	}
	t.Fatal("account not listed")
	return ManagedAccount{}
}

// freezeStore makes the store unwritable once the next consume arrives.
func (r resetRig) freezeStore(t *testing.T) {
	t.Helper()
	r.write(t, "freeze-store", r.storeDir)
	t.Cleanup(func() { _ = os.Chmod(r.storeDir, 0o700) })
}

func publicCode(err error) string {
	code, _, _ := errorsx.PublicDetails(err)
	return code
}

func (r resetRig) resets(t *testing.T) *provideraccounts.UsageResets {
	t.Helper()
	account, ok := r.store.Get(codexName, "inactive", time.Now())
	if !ok {
		t.Fatal("account missing")
	}
	return account.UsageResets
}

// newResetRig builds a manager over its own store; restartOf builds the one a
// restart of that rig's app would: a new store read from the same file, over
// the same credentials.
func newResetRig(t *testing.T, restartOf *resetRig) resetRig {
	t.Helper()
	_, _, creds := newTestManager(t)
	storeDir := t.TempDir()
	if restartOf != nil {
		creds, storeDir = restartOf.creds, restartOf.storeDir
	}
	store, err := provideraccounts.NewStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{
		Reaper:          procutiltest.Reaper(),
		ClaudeRetention: func(string) claude.Retention { return claude.UserHomeRetention(0) },
	})
	if err := manager.Attach(store, creds, ""); err != nil {
		t.Fatal(err)
	}
	rig := resetRig{dir: t.TempDir(), storeDir: storeDir, manager: manager, store: store, creds: creds, sink: &recordingAccountSink{}}
	manager.deps.Accounts = rig.sink
	binary := mockexec.Write(t, rig.file("mock-codex"), "#!/bin/bash\n"+
		"dir="+shellQuoteForTest(rig.dir)+"\n"+
		"while IFS= read -r line; do\n"+
		`case "$line" in`+"\n"+
		`*'"account/read"'*) [ -f "$dir/exit-at-account" ] && exit 0; cat "$dir/account.json" ;;`+"\n"+
		`*rateLimitResetCredit/consume*)`+"\n"+
		`  printf '%s\n' "$line" >> "$dir/consumes.log"`+"\n"+
		`  [ -f "$dir/freeze-store" ] && chmod 0500 "$(cat "$dir/freeze-store")"`+"\n"+
		`  [ -s "$dir/consume.json" ] || exit 0`+"\n"+
		`  cat "$dir/consume.json" ;;`+"\n"+
		`*'"account/rateLimits/read"'*)`+"\n"+
		`  cp `+shellQuoteForTest(filepath.Join(storeDir, "provider-accounts.json"))+` "$dir/store-at-reading.json"`+"\n"+
		`  [ -f "$dir/rotate.json" ] && cp "$dir/rotate.json" "$CODEX_HOME/auth.json"`+"\n"+
		`  [ -s "$dir/ratelimits.json" ] || exit 0`+"\n"+
		`  cat "$dir/ratelimits.json" ;;`+"\n"+
		"esac\ndone\n")
	manager.deps.ProviderBinary = func(string) string { return binary }
	rig.write(t, "account.json", chatgptAccountFrame("inactive@example.com", "team", "")+"\n")
	rig.holdResets(t, "null")
	return rig
}

// seedResetAccount saves the inactive account the rig spends from, with the
// list its first reading names.
func seedResetAccount(t *testing.T, rig resetRig, summary string) *provideraccounts.UsageResets {
	t.Helper()
	if _, err := rig.store.Add(provideraccounts.Account{
		ID: "inactive", Provider: codexName, Email: "inactive@example.com", OrgID: "ws-inactive", SubscriptionType: "team",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.creds.WriteAccountCredential(codexName, "inactive", []byte(inactiveCredential)); err != nil {
		t.Fatal(err)
	}
	rig.holdResets(t, summary)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatalf("first reading: %v", err)
	}
	resets := rig.resets(t)
	if resets == nil {
		t.Fatal("the first reading recorded no reset list")
	}
	return resets
}

// inactiveCredential is the inactive account's login, on its workspace.
const inactiveCredential = `{"tokens":{"access_token":"inactive","refresh_token":"inactive-refresh","account_id":"ws-inactive"}}`

const twoCredits = `{"availableCount":2,"credits":[` +
	`{"id":"RateLimitResetCredit_late","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":3000,"title":null,"description":null},` +
	`{"id":"RateLimitResetCredit_soon","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":2000,"title":"Full reset (Weekly + 5 hr)","description":"Resets your weekly and 5 hour limits."}]}`

const lateCreditOnly = `{"availableCount":1,"credits":[` +
	`{"id":"RateLimitResetCredit_late","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":3000,"title":null,"description":null}]}`

func TestCodexReadingsRecordTheirResetsAndPublishOnlyAChange(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	want := []provideraccounts.UsageResetOption{
		{ID: "RateLimitResetCredit_soon", Kind: "codexRateLimits", Label: "Full reset (Weekly + 5 hr)", Refills: "Resets your weekly and 5 hour limits.", ExpiresAt: 2000, Usable: true},
		{ID: "RateLimitResetCredit_late", Kind: "codexRateLimits", Label: codexResetLabel, Refills: codexResetRefills, ExpiresAt: 3000, Usable: true},
	}
	if len(resets.Options) != 2 || resets.Options[0] != want[0] || resets.Options[1] != want[1] {
		t.Fatalf("options = %+v, want %+v", resets.Options, want)
	}
	if len(rig.sink.listings) != 1 {
		t.Fatalf("listing changes = %v, want one", rig.sink.listings)
	}
	// The same list again publishes nothing.
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatal(err)
	}
	// A reading that names no resets says nothing about them: the list stays.
	rig.holdResets(t, "null")
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatal(err)
	}
	if got := rig.resets(t); got.Revision != resets.Revision || len(got.Options) != 2 {
		t.Fatalf("after a reading without resets: %+v", got)
	}
	if len(rig.sink.listings) != 1 {
		t.Fatalf("listing changes = %v, want still one", rig.sink.listings)
	}
	if len(rig.sink.changed) != 0 {
		t.Fatalf("a reset list change published the account set as changed: %v", rig.sink.changed)
	}
}

// Every option names its credit, carries Codex's kind, and the credits Codex
// counted beyond its capped list are a count no option claims.
func TestCodexResetOptionsNameEveryCreditAndCountTheUnlisted(t *testing.T) {
	for name, tc := range map[string]struct {
		summary  string
		want     []provideraccounts.UsageResetOption
		unlisted int
	}{
		"capped list": {
			summary: `{"availableCount":3,"credits":[` +
				`{"id":"RateLimitResetCredit_1","resetType":"unknown","status":"redeeming","grantedAt":1,"expiresAt":null,"title":null,"description":null},` +
				`{"id":"RateLimitResetCredit_3","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":3000,"title":null,"description":null},` +
				`{"id":"RateLimitResetCredit_2","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":2000,"title":"Weekly","description":null}]}`,
			want: []provideraccounts.UsageResetOption{
				{ID: "RateLimitResetCredit_2", Kind: "codexRateLimits", Label: "Weekly", Refills: codexResetRefills, ExpiresAt: 2000, Usable: true},
				{ID: "RateLimitResetCredit_3", Kind: "codexRateLimits", Label: codexResetLabel, Refills: codexResetRefills, ExpiresAt: 3000, Usable: true},
				{ID: "RateLimitResetCredit_1", Kind: "unknown", Label: codexResetLabel, Refills: codexResetRefills, Reason: "Codex is still applying this reset."},
			},
			unlisted: 1,
		},
		// A count with no list is shown as a count, with nothing to spend.
		"count only": {
			summary:  `{"availableCount":2,"credits":null}`,
			unlisted: 2,
		},
		"none usable listed": {
			summary: `{"availableCount":1,"credits":[` +
				`{"id":"RateLimitResetCredit_1","resetType":"codexRateLimits","status":"redeeming","grantedAt":1,"expiresAt":null,"title":null,"description":null}]}`,
			want: []provideraccounts.UsageResetOption{
				{ID: "RateLimitResetCredit_1", Kind: "codexRateLimits", Label: codexResetLabel, Refills: codexResetRefills, Reason: "Codex is still applying this reset."},
			},
			unlisted: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newResetRig(t, nil)
			resets := seedResetAccount(t, rig, tc.summary)
			got := resets.Options
			if len(got) != len(tc.want) || resets.Unlisted != tc.unlisted {
				t.Fatalf("options = %+v, unlisted %d; want %+v, unlisted %d", got, resets.Unlisted, tc.want, tc.unlisted)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("option %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A count the card cannot show as Codex gave it is logged once per change,
// not on every poll.
func TestCodexResetCountsThatDisagreeAreLoggedOncePerChange(t *testing.T) {
	var logged strings.Builder
	output, flags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(output); log.SetFlags(flags) })
	manager := NewManager(Deps{})
	read := func(summary codex.ResetCredits) {
		manager.mu.Lock()
		manager.codexResetReadingLocked("acct", &summary, "")
		manager.mu.Unlock()
	}
	// A count with no list is shown as it is, so it is not a disagreement.
	countOnly := codex.ResetCredits{AvailableCount: 2}
	overListed := codex.ResetCredits{AvailableCount: 0, Credits: []codex.ResetCredit{{ID: "c", Status: codex.ResetCreditAvailable}}}
	consistent := codex.ResetCredits{AvailableCount: 1, Credits: []codex.ResetCredit{{ID: "c", Status: codex.ResetCreditAvailable}}}
	for _, summary := range []codex.ResetCredits{countOnly, overListed, overListed, consistent, countOnly, overListed} {
		read(summary)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "listed 1") || !strings.Contains(lines[1], "listed 1") {
		t.Fatalf("logged %q, want each disagreement once per change", lines)
	}
}

func TestSpendProviderAccountResetSpendsTheChosenResetThenReads(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.holdResets(t, lateCreditOnly)
	result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err != nil {
		t.Fatalf("SpendProviderAccountReset: %v", err)
	}
	if !result.Spent || !result.ReadingLanded || result.Message == "" {
		t.Fatalf("result = %+v", result)
	}
	consumes := rig.consumes(t)
	if len(consumes) != 1 || consumes[0]["creditId"] != "RateLimitResetCredit_soon" || consumes[0]["idempotencyKey"] == "" {
		t.Fatalf("consumes = %v", consumes)
	}
	after := rig.resets(t)
	if after.Revision <= resets.Revision || len(after.Options) != 1 || after.Options[0].ID != "RateLimitResetCredit_late" {
		t.Fatalf("after the spend: %+v", after)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a settled spend stayed unsettled")
	}
	if rig.listed(t).PendingReset != nil {
		t.Fatal("the listing offers Finish for a settled spend")
	}
	if len(rig.sink.limitsReset) != 1 || rig.sink.limitsReset[0] != "inactive" {
		t.Fatalf("limits reset published for %v, want the account once", rig.sink.limitsReset)
	}
	// Settled on receipt: the store had ended the spend, and dropped its
	// credit, before the reading after it was asked for.
	saved, err := os.ReadFile(rig.file("store-at-reading.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), `"pendingResets"`) || strings.Contains(string(saved), "RateLimitResetCredit_soon") {
		t.Fatalf("the store at the reading after the answer still held the spend: %s", saved)
	}
}

// A reset whose reading after it did not land leaves the refusals as they
// are: nothing says what the account has left.
func TestSpendProviderAccountResetWithoutAReadingClearsNoRefusals(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.write(t, "ratelimits.json", `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team"},"rateLimitResetCredits":`+lateCreditOnly+`}}`+"\n")
	result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err != nil || !result.Spent || result.ReadingLanded || !strings.Contains(result.Message, "Reset used") ||
		!strings.Contains(result.Message, "refresh the account") {
		t.Fatalf("result = %+v, %v; want the spend's answer with the failed reading beside it", result, err)
	}
	if len(rig.sink.limitsReset) != 0 {
		t.Fatalf("limits reset published without a reading: %v", rig.sink.limitsReset)
	}
	if got := rig.resets(t); got.Revision <= resets.Revision {
		t.Fatalf("a spend without a reading left the revision at %d", got.Revision)
	}
}

func TestSpendProviderAccountResetRefusesAListThePersonDidNotSee(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	for name, spend := range map[string]struct {
		option   string
		revision uint64
	}{
		"old revision":   {"RateLimitResetCredit_soon", resets.Revision - 1},
		"unknown option": {"RateLimitResetCredit_gone", resets.Revision},
	} {
		if _, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", spend.option, spend.revision); !errors.Is(err, ErrResetListChanged) {
			t.Fatalf("%s: err = %v, want ErrResetListChanged", name, err)
		}
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a refused spend reached Codex: %v", consumes)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a refused spend was recorded")
	}
}

func TestSpendProviderAccountResetRefusesAResetThatCantBeUsedNow(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, `{"availableCount":0,"credits":[`+
		`{"id":"RateLimitResetCredit_1","resetType":"codexRateLimits","status":"redeeming","grantedAt":1,"expiresAt":null,"title":null,"description":null}]}`)
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_1", resets.Revision)
	if err == nil || !strings.Contains(err.Error(), "still applying") {
		t.Fatalf("err = %v, want the reason it can't be used", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("an unusable reset was sent: %v", consumes)
	}
}

// The lost-answer sequence: the spend stays unsettled through a reading that
// drops its credit, no other spend goes out meanwhile, and Finish sends the
// same key and credit and settles it.
func TestALostAnswerStaysUnsettledUntilFinishSettlesIt(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)

	rig.answerConsumeFrame(t, "") // the process exits before answering
	listings := len(rig.sink.listings)
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if len(rig.sink.listings) != listings+1 {
		t.Fatalf("listing changes after a begun spend = %v, want one more", rig.sink.listings)
	}
	if publicCode(err) != "reset_unconfirmed" || !strings.Contains(err.Error(), "Press Finish on the account") ||
		strings.Contains(err.Error(), "Try again") {
		t.Fatalf("lost answer: err = %v", err)
	}
	pending, ok := rig.pending(t)
	if !ok || pending.CreditID != "RateLimitResetCredit_soon" {
		t.Fatalf("after the lost answer: pending = %+v ok=%v", pending, ok)
	}
	card := rig.listed(t).PendingReset
	if card == nil || card.Label != "Full reset (Weekly + 5 hr)" || card.StartedAt != pending.StartedAt || card.ID != pending.ID {
		t.Fatalf("listing's unsettled reset = %+v", card)
	}
	wire, err := json.Marshal(rig.listed(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), pending.Key) {
		t.Fatalf("the listing carries the spend's key: %s", wire)
	}

	// A reading that no longer lists the credit says nothing about whether
	// this spend took it.
	rig.holdResets(t, lateCreditOnly)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rig.pending(t); !ok {
		t.Fatal("a reading ended the unsettled spend")
	}
	// A Use of the other credit, from a card that saw the current list, is
	// refused unsent and names the unsettled reset.
	_, err = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_late", rig.resets(t).Revision)
	if publicCode(err) != "reset_unsettled" || !strings.Contains(err.Error(), "Full reset (Weekly + 5 hr)") {
		t.Fatalf("a Use while unsettled: err = %v", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 1 {
		t.Fatalf("a Use while unsettled was sent: %v", consumes)
	}

	rig.answerConsume(t, "alreadyRedeemed")
	listings = len(rig.sink.listings)
	result, err := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", card.ID)
	if err != nil || !result.Spent || !strings.Contains(result.Message, "already used") {
		t.Fatalf("Finish = %+v, %v", result, err)
	}
	if len(rig.sink.listings) != listings+1 {
		t.Fatalf("listing changes after Finish = %v, want one more", rig.sink.listings)
	}
	consumes := rig.consumes(t)
	if len(consumes) != 2 || consumes[1]["idempotencyKey"] != pending.Key || consumes[1]["creditId"] != "RateLimitResetCredit_soon" {
		t.Fatalf("consumes = %v, want the first request again", consumes)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a settled Finish left the spend unsettled")
	}
	// The limits were reset by the first request, not by this one.
	if len(rig.sink.limitsReset) != 0 {
		t.Fatalf("limits reset published %v for an alreadyRedeemed Finish", rig.sink.limitsReset)
	}
}

// After a restart the saved spend is still offered, and Finish sends its key.
func TestFinishAfterARestartSendsTheSavedKey(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsumeFrame(t, "")
	if _, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision); err == nil {
		t.Fatal("a lost answer reported success")
	}
	pending, _ := rig.pending(t)

	restarted := newResetRig(t, &rig)
	card := restarted.listed(t).PendingReset
	if card == nil || card.ID != pending.ID {
		t.Fatalf("after a restart the listing offers %+v, want Finish of the saved spend", card)
	}
	restarted.answerConsume(t, "alreadyRedeemed")
	restarted.holdResets(t, lateCreditOnly)
	result, err := restarted.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", card.ID)
	if err != nil || !result.Spent {
		t.Fatalf("Finish after restart = %+v, %v", result, err)
	}
	if consumes := restarted.consumes(t); len(consumes) != 1 || consumes[0]["idempotencyKey"] != pending.Key ||
		consumes[0]["creditId"] != pending.CreditID {
		t.Fatalf("consumes after restart = %v, want key %q", consumes, pending.Key)
	}
	if _, ok := restarted.pending(t); ok {
		t.Fatal("Finish after a restart left the spend unsettled")
	}
}

// A spend whose record cannot be saved is never sent.
func TestASpendThatCannotBeRecordedIsNotSent(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	if err := os.Chmod(rig.storeDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rig.storeDir, 0o700) })
	if _, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision); err == nil {
		t.Fatal("a spend whose record failed to save reported success")
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("an unrecorded spend was sent: %v", consumes)
	}
}

// A crash between Codex's answer and the save that settles it leaves the
// spend unsettled on disk: after a restart the list it was chosen from
// cannot spend, and Finish sends its key.
func TestACrashBeforeTheSettleSaveLeavesTheSpendToFinish(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.holdResets(t, lateCreditOnly)
	rig.freezeStore(t)
	result, _ := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if !result.Spent {
		t.Fatalf("result = %+v, want Codex's answer", result)
	}
	pending, ok := rig.pending(t)
	if !ok {
		t.Fatal("a settle that failed to save ended the spend")
	}
	if err := os.Chmod(rig.storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rig.file("freeze-store")); err != nil {
		t.Fatal(err)
	}

	restarted := newResetRig(t, &rig)
	restarted.answerConsume(t, "reset")
	for _, revision := range []uint64{resets.Revision, restarted.resets(t).Revision} {
		if _, err := restarted.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", revision); err == nil {
			t.Fatalf("a spend from revision %d went ahead of the unsettled one", revision)
		}
	}
	if consumes := restarted.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend went out after the crash: %v", consumes)
	}
	if restarted.listed(t).PendingReset == nil {
		t.Fatal("after the crash the listing offers no Finish")
	}
	restarted.answerConsume(t, "alreadyRedeemed")
	if _, err := restarted.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", pending.ID); err != nil {
		t.Fatal(err)
	}
	if consumes := restarted.consumes(t); len(consumes) != 1 || consumes[0]["idempotencyKey"] != pending.Key {
		t.Fatalf("Finish after the crash sent %v, want key %q", consumes, pending.Key)
	}
}

// A refusal of Finish settles only that request: the first may have landed.
func TestFinishRefusedKeepsTheSpendAndAnsweredSettlesIt(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsumeFrame(t, "")
	_, _ = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)

	spendID := rig.listed(t).PendingReset.ID
	rig.answerConsumeFrame(t, `{"jsonrpc":"2.0","id":4,"error":{"code":-32600,"message":"chatgpt authentication required"}}`+"\n")
	_, err := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", spendID)
	if err == nil || !strings.Contains(err.Error(), "The first request may still have used the reset") ||
		strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("refused Finish: err = %v", err)
	}
	if _, ok := rig.pending(t); !ok {
		t.Fatal("a refused Finish ended the spend")
	}
	// Another login refuses Finish unsent, in the same words.
	rig.write(t, "account.json", chatgptAccountFrame("someone-else@example.com", "team", "")+"\n")
	_, err = rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", spendID)
	if publicCode(err) != "reset_account_mismatch" || !strings.Contains(err.Error(), "The first request may still have used the reset") {
		t.Fatalf("Finish under another login: err = %v", err)
	}
	if _, ok := rig.pending(t); !ok {
		t.Fatal("a Finish that was never sent ended the spend")
	}
	rig.write(t, "account.json", chatgptAccountFrame("inactive@example.com", "team", "")+"\n")

	rig.answerConsume(t, "noCredit")
	result, err := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", spendID)
	if err != nil || result.Spent || result.Message != "This reset is no longer available: the first request used it, or it expired." {
		t.Fatalf("Finish answered noCredit = %+v, %v", result, err)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("an answered Finish left the spend unsettled")
	}
}

// With nothing unsettled Finish sends nothing; two Finish calls take the lock
// in turn and the second finds the spend settled.
func TestFinishWithNothingUnsettledSendsNothing(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	if _, err := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", "any"); !errors.Is(err, errResetNothingUnsettled) {
		t.Fatalf("Finish with nothing unsettled: err = %v", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("Finish with nothing unsettled sent %v", consumes)
	}

	rig.answerConsumeFrame(t, "")
	_, _ = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	rig.answerConsume(t, "alreadyRedeemed")
	spendID := rig.listed(t).PendingReset.ID
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", spendID)
		})
	}
	wg.Wait()
	settled, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			settled++
		case errors.Is(err, errResetNothingUnsettled):
			refused++
		default:
			t.Fatalf("Finish: %v", err)
		}
	}
	if settled != 1 || refused != 1 {
		t.Fatalf("two Finish calls: %d settled, %d refused; want one each", settled, refused)
	}
	if consumes := rig.consumes(t); len(consumes) != 2 {
		t.Fatalf("consumes = %v, want the first request and one Finish", consumes)
	}
}

// A fresh spend whose own request was refused used nothing, so it ends.
func TestARefusedSpendEndsAndSaysNothingWasUsed(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsumeFrame(t, `{"jsonrpc":"2.0","id":4,"error":{"code":-32600,"message":"chatgpt authentication required for rate limit reset credits"}}`+"\n")
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err == nil || !strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("refusal: err = %v", err)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a refused spend stayed unsettled")
	}
}

func TestSpendProviderAccountResetStillCommitsTheRotationItsReadingMade(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rotated := `{"tokens":{"access_token":"rotated","refresh_token":"rotated-refresh"},"last_refresh":"2030-01-01T00:00:00Z"}`
	rig.write(t, "rotate.json", rotated)
	rig.answerConsume(t, "reset")
	rig.holdResets(t, lateCreditOnly)
	if _, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision); err != nil {
		t.Fatal(err)
	}
	slot, err := rig.creds.ReadCredentialSnapshot(codexName, "inactive", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(slot.Bytes()) != rotated {
		t.Fatalf("slot after a spend = %s, want the rotation", slot.Bytes())
	}
}

// The "reset" outcome is TestSpendProviderAccountResetSpendsTheChosenResetThenReads.
func TestSpendProviderAccountResetMessagesEveryOutcome(t *testing.T) {
	for outcome, wantSpent := range map[string]bool{
		"alreadyRedeemed": true, "nothingToReset": false, "noCredit": false,
	} {
		t.Run(outcome, func(t *testing.T) {
			rig := newResetRig(t, nil)
			resets := seedResetAccount(t, rig, twoCredits)
			rig.answerConsume(t, outcome)
			result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
			if err != nil || result.Spent != wantSpent || result.Message == "" || !result.ReadingLanded {
				t.Fatalf("result = %+v, %v; want spent=%v", result, err, wantSpent)
			}
			// Only this request's own reset clears the refusals.
			if len(rig.sink.limitsReset) != 0 {
				t.Fatalf("limits reset published = %v for %s", rig.sink.limitsReset, outcome)
			}
			if _, ok := rig.pending(t); ok {
				t.Fatal("an answered spend stayed unsettled")
			}
		})
	}
}

func TestSpendProviderAccountResetRefusesClaude(t *testing.T) {
	rig := newResetRig(t, nil)
	if _, err := rig.manager.SpendProviderAccountReset(t.Context(), string(provider.Claude), "any", "", 1); err == nil {
		t.Fatal("a Claude spend was accepted")
	}
	if _, err := rig.manager.FinishProviderAccountReset(t.Context(), string(provider.Claude), "any", "spend"); err == nil {
		t.Fatal("a Claude Finish was accepted")
	}
}

// The consume goes out only under the login the person chose: an app-server
// signed in as someone else is refused before anything is sent.
func TestSpendProviderAccountResetSendsNothingUnderAnotherLogin(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.write(t, "account.json", chatgptAccountFrame("someone-else@example.com", "team", "")+"\n")
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err == nil || !strings.Contains(err.Error(), "so the reset wasn't sent. No reset was used") {
		t.Fatalf("err = %v, want the refusal naming the login", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend went out under another login: %v", consumes)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("a spend that was never sent stayed unsettled")
	}
}

// A spend waits a bounded time behind another account operation, and one
// that could not start goes out never, not after its caller gave up.
func TestSpendProviderAccountResetRefusesUnsentWhenTheLockStaysHeld(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.manager.resetLockWaitOverride = 50 * time.Millisecond
	held := rig.manager.reconcileMutex(codexName)
	if err := held.LockContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	_, finishErr := rig.manager.FinishProviderAccountReset(t.Context(), codexName, "inactive", "spend")
	held.Unlock()
	if publicCode(err) != "reset_busy" || !strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("err = %v, want the unsent refusal", err)
	}
	if publicCode(finishErr) != "reset_busy" || !strings.Contains(finishErr.Error(), "press Finish again") {
		t.Fatalf("Finish err = %v, want the unsent refusal for Finish", finishErr)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("a spend that could not take the lock was sent: %v", consumes)
	}
}

// What the CLI rotated in the temporary home reaches the slot even when the
// reading then fails, on a plain refresh and after a spend.
func TestCodexUsageRotationReachesTheSlotWhenTheReadingFails(t *testing.T) {
	rotated := `{"tokens":{"access_token":"rotated","refresh_token":"rotated-refresh"},"last_refresh":"2030-01-01T00:00:00Z"}`
	for _, spend := range []bool{false, true} {
		rig := newResetRig(t, nil)
		resets := seedResetAccount(t, rig, twoCredits)
		rig.write(t, "rotate.json", rotated)
		rig.write(t, "ratelimits.json", "")
		rig.answerConsume(t, "reset")
		var err error
		if spend {
			var result ResetSpendResult
			result, err = rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
			if !result.Spent || result.ReadingLanded {
				t.Fatalf("spend result = %+v", result)
			}
		} else {
			err = rig.manager.RefreshProviderAccountUsage(codexName, "inactive")
			if err == nil {
				t.Fatal("a refresh whose reading failed reported no error")
			}
		}
		slot, readErr := rig.creds.ReadCredentialSnapshot(codexName, "inactive", false)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(slot.Bytes()) != rotated {
			t.Fatalf("spend=%v (err %v): slot = %s, want the rotation", spend, err, slot.Bytes())
		}
	}
}

// An answer without usage windows still names the resets, and they land.
func TestCodexResetsLandFromAReadingWithoutUsageWindows(t *testing.T) {
	rig := newResetRig(t, nil)
	seedResetAccount(t, rig, twoCredits)
	rig.write(t, "ratelimits.json", `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team"},"rateLimitResetCredits":`+lateCreditOnly+`}}`+"\n")
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err == nil {
		t.Fatal("a reading without usage windows reported no error")
	}
	if got := rig.resets(t); len(got.Options) != 1 || got.Options[0].ID != "RateLimitResetCredit_late" {
		t.Fatalf("resets after a reading without windows = %+v", got)
	}
}

// A reading after a spend that still lists the credit Codex's answer says is
// gone is stale about it: the credit is neither offered nor counted again.
func TestAReadingAfterASpendThatStillListsItsCreditDoesNotOfferItAgain(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err != nil || !result.Spent || !result.ReadingLanded {
		t.Fatalf("result = %+v, %v", result, err)
	}
	after := rig.resets(t)
	if len(after.Options) != 1 || after.Options[0].ID != "RateLimitResetCredit_late" || after.Unlisted != 0 {
		t.Fatalf("after a spend whose reading still listed its credit: %+v", after)
	}
	// The next ordinary reading is the list again.
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatal(err)
	}
	if got := rig.resets(t); len(got.Options) != 2 {
		t.Fatalf("the next reading's list = %+v, want Codex's two", got)
	}
}

// Codex's answer settles the spend on receipt: a reading after it that the
// store refuses (an id named twice) does not keep the spend unsettled, and is
// not recorded; the next valid reading replaces the list.
func TestAnInvalidReadingAfterASpendDoesNotKeepItUnsettled(t *testing.T) {
	rig := newResetRig(t, nil)
	resets := seedResetAccount(t, rig, twoCredits)
	rig.answerConsume(t, "reset")
	rig.holdResets(t, `{"availableCount":2,"credits":[`+
		`{"id":"RateLimitResetCredit_late","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":3000,"title":null,"description":null},`+
		`{"id":"RateLimitResetCredit_late","resetType":"codexRateLimits","status":"available","grantedAt":1,"expiresAt":3000,"title":null,"description":null}]}`)
	result, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "inactive", "RateLimitResetCredit_soon", resets.Revision)
	if err != nil || !result.Spent || !strings.Contains(result.Message, "Reset used") {
		t.Fatalf("result = %+v, %v; want Codex's answer", result, err)
	}
	if _, ok := rig.pending(t); ok {
		t.Fatal("an invalid reading after the answer kept the spend unsettled")
	}
	if after := rig.resets(t); len(after.Options) != 1 || after.Options[0].ID != "RateLimitResetCredit_late" {
		t.Fatalf("after the spend: %+v, want the settled list without the spent credit", after)
	}
	rig.holdResets(t, twoCredits)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "inactive"); err != nil {
		t.Fatal(err)
	}
	if got := rig.resets(t); len(got.Options) != 2 {
		t.Fatalf("the next valid reading's list = %+v, want Codex's two", got)
	}
}
