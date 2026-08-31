package identity

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
)

func TestRecoveryCodesAreMintedShownOnceAndSpentOnce(t *testing.T) {
	sessions, _, _, owner, device := newFixture(t)
	codes, err := sessions.MintRecoveryCodes(owner.ID)
	if err != nil {
		t.Fatalf("MintRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("minted %d codes, want %d", len(codes), RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if seen[code] {
			t.Fatalf("the same code was minted twice: %q", code)
		}
		seen[code] = true
		if strings.Count(code, "-") != recoveryCodeGroups-1 {
			t.Fatalf("code %q is not grouped for reading", code)
		}
	}

	// A code minted works exactly as displayed — the dashes and the case
	// are presentation, and the hash is over the normalized form.
	got, err := sessions.ConsumeRecoveryCode(codes[0], device.ID, "127.0.0.1")
	if err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if got != owner.ID {
		t.Fatalf("code admitted %q, want the owner", got)
	}
	// The replay.
	if _, err := sessions.ConsumeRecoveryCode(codes[0], device.ID, "127.0.0.1"); !errors.Is(err, ErrRecoveryCodeRefused) {
		t.Fatalf("replayed code = %v, want ErrRecoveryCodeRefused", err)
	}
	// A code that never existed answers identically, because the backend
	// genuinely cannot tell the two apart.
	if _, err := sessions.ConsumeRecoveryCode("ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ", "", ""); !errors.Is(err, ErrRecoveryCodeRefused) {
		t.Fatalf("unknown code = %v, want ErrRecoveryCodeRefused", err)
	}
}

func TestRecoveryCodeAcceptsEverySpellingSomeoneWouldType(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	codes, err := sessions.MintRecoveryCodes(owner.ID)
	if err != nil {
		t.Fatalf("MintRecoveryCodes: %v", err)
	}
	spellings := map[string]string{
		"as displayed": codes[0],
		"lower case":   strings.ToLower(codes[1]),
		"no dashes":    strings.ReplaceAll(codes[2], "-", ""),
		"spaces":       strings.ReplaceAll(codes[3], "-", " "),
		"padded":       "  " + codes[4] + "  ",
	}
	for name, spelling := range spellings {
		if _, err := sessions.ConsumeRecoveryCode(spelling, "", ""); err != nil {
			t.Fatalf("%s: ConsumeRecoveryCode(%q): %v", name, spelling, err)
		}
	}
}

func TestNormalizeRecoveryCodeRefusesWhatCannotBeACode(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"too short":          "ABCDE-ABCDE-ABCDE",
		"too long":           "ABCDE-ABCDE-ABCDE-ABCDE-ABCDE",
		"ambiguous letter I": "ABCDI-ABCDE-ABCDE-ABCDE",
		"ambiguous letter O": "ABCDO-ABCDE-ABCDE-ABCDE",
		"punctuation":        "ABCD!-ABCDE-ABCDE-ABCDE",
	}
	for name, input := range cases {
		if got := NormalizeRecoveryCode(input); got != "" {
			t.Fatalf("%s: NormalizeRecoveryCode(%q) = %q, want a refusal", name, input, got)
		}
	}
	if got := NormalizeRecoveryCode("abcde-fghjk-mnpqr-stvwx"); got != "ABCDEFGHJKMNPQRSTVWX" {
		t.Fatalf("NormalizeRecoveryCode = %q", got)
	}
}

// TestRecoveryCodeConsumptionHasOneWinner — several devices presenting the
// same code at once is a real race (a person tapping twice, a retry), and
// exactly one must succeed.
func TestRecoveryCodeConsumptionHasOneWinner(t *testing.T) {
	sessions, _, _, owner, _ := newFixture(t)
	codes, err := sessions.MintRecoveryCodes(owner.ID)
	if err != nil {
		t.Fatalf("MintRecoveryCodes: %v", err)
	}
	const callers = 8
	var winners atomic.Int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			if _, err := sessions.ConsumeRecoveryCode(codes[0], "", ""); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("%d callers spent the same code, want exactly 1", got)
	}
}

func TestRecoveryCodeEventsAreAudited(t *testing.T) {
	sessions, st, _, owner, device := newFixture(t)
	codes, err := sessions.MintRecoveryCodes(owner.ID)
	if err != nil {
		t.Fatalf("MintRecoveryCodes: %v", err)
	}
	if _, err := sessions.ConsumeRecoveryCode(codes[0], device.ID, "10.0.0.4"); err != nil {
		t.Fatalf("ConsumeRecoveryCode: %v", err)
	}
	if _, err := sessions.ConsumeRecoveryCode(codes[0], device.ID, "10.0.0.4"); err == nil {
		t.Fatal("a replay succeeded")
	}
	entries, err := st.ListRecentAuthAudit(50)
	if err != nil {
		t.Fatalf("ListRecentAuthAudit: %v", err)
	}
	seen := map[string]store.AuthAuditEntry{}
	for _, entry := range entries {
		seen[entry.Event] = entry
	}
	for _, want := range []AuditEvent{
		AuditRecoveryCodesMinted, AuditRecoveryCodeConsumed, AuditRecoveryCodeRefused,
	} {
		if _, ok := seen[string(want)]; !ok {
			t.Fatalf("no %q entry in the credential log", want)
		}
	}
	if seen[string(AuditRecoveryCodeRefused)].Outcome != store.AuthAuditOutcomeRefused {
		t.Fatal("a refused code was logged as allowed")
	}
	if seen[string(AuditRecoveryCodeConsumed)].Peer != "10.0.0.4" {
		t.Fatal("consumption was logged without peer attribution")
	}
}

func TestMintRecoveryCodesRefusesAnEmptyAccount(t *testing.T) {
	sessions, _, _, _, _ := newFixture(t)
	if _, err := sessions.MintRecoveryCodes(""); err == nil {
		t.Fatal("MintRecoveryCodes accepted an empty user id")
	}
}

// TestNewRecoveryCodeReturnsBothForms — the display form carries the
// dashes and the normalized form is what gets hashed. Only the second may
// reach the store, or the codes shown would verify against nothing.
func TestNewRecoveryCodeReturnsBothForms(t *testing.T) {
	display, normalized, err := newRecoveryCode()
	if err != nil {
		t.Fatalf("newRecoveryCode: %v", err)
	}
	if NormalizeRecoveryCode(display) != normalized {
		t.Fatalf("normalizing %q gives %q, but the mint hashed %q",
			display, NormalizeRecoveryCode(display), normalized)
	}
	if strings.Contains(normalized, "-") {
		t.Fatalf("the normalized form still carries separators: %q", normalized)
	}
	if len(normalized) != recoveryCodeGroups*recoveryCodeGroupLen {
		t.Fatalf("normalized form is %d characters", len(normalized))
	}
	for _, r := range normalized {
		if !strings.ContainsRune(recoveryAlphabet, r) {
			t.Fatalf("code carries %q, which is outside the alphabet", r)
		}
	}
}
