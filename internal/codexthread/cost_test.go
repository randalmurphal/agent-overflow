package codexthread

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

const costTestProjectID = "codexthread-cost-test"

type costTestHarness struct {
	*Service
	sessions map[string]LiveSession
}

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

func newCostTestHarness(t *testing.T) *costTestHarness {
	t.Helper()
	st := storetest.Clone(t)
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{
		ID: costTestProjectID, Path: "/tmp/codexthread", Name: "Codex Thread Test",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	h := &costTestHarness{sessions: make(map[string]LiveSession)}
	h.Service = New(Deps{
		Context: func() context.Context { return context.Background() },
		Store:   st,
		Session: func(threadID string) (LiveSession, bool) {
			session, ok := h.sessions[threadID]
			return session, ok
		},
	})
	return h
}

func newCostTestHarnessWithPath(t *testing.T) (*costTestHarness, string) {
	t.Helper()
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{
		ID: costTestProjectID, Path: "/tmp/codexthread", Name: "Codex Thread Test",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	h := &costTestHarness{sessions: make(map[string]LiveSession)}
	h.Service = New(Deps{Store: st, Session: func(threadID string) (LiveSession, bool) {
		session, ok := h.sessions[threadID]
		return session, ok
	}})
	return h, path
}

func testCostThread(id string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID: id, ProjectID: costTestProjectID, Title: "Test Thread",
		Provider: string(provider.Codex), WorkspacePath: "/tmp/codexthread",
		Model: "gpt-5.4", Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}
}

// These test-only names preserve the domain language in the original
// concurrency regression suite while production uses the shorter service
// implementation names.
func (a *Service) claimCodexThreadCostRead(threadID, sessionToken string) (uint64, bool) {
	return a.claimThreadCostRead(threadID, sessionToken)
}

func (a *Service) nextCodexThreadCostRead(threadID string) (string, uint64, bool) {
	return a.nextThreadCostRead(threadID)
}

func (a *Service) codexThreadCostReadIsCurrent(threadID string, epoch uint64) bool {
	return a.threadCostReadIsCurrent(threadID, epoch)
}

func (a *Service) persistCodexThreadCostIfCurrent(threadID string, epoch uint64, cost store.ProviderThreadCost) (bool, error) {
	return a.persistThreadCostIfCurrent(threadID, epoch, cost)
}

func (a *Service) forgetCodexThreadCost(threadID string) {
	a.ForgetThreadCost(threadID)
}

func (a *Service) readCodexThreadCost(threadID, sessionToken string, epoch uint64) {
	a.readThreadCost(threadID, sessionToken, epoch)
}

func (a *Service) overlayProviderThreadCost(query store.UsageQuery, buckets []store.UsageBucket) []store.UsageBucket {
	return a.OverlayProviderThreadCost(query, buckets)
}

// codexCostThread is testThread already pointing at a Codex thread. Every
// stored provider figure names the provider thread it was read from, and
// GetProviderThreadCost only answers while that still matches the thread's own
// SessionRef (migration v68), so a thread with no ref can hold no readable row.
func codexCostThread(id, sessionRef string) store.Thread {
	thread := testCostThread(id)
	thread.SessionRef = sessionRef
	return thread
}

// B5/B14: the per-thread read slot. Two turns settling around one in-flight
// read must not each fire their own request, and must not be DROPPED either:
// the request already out may have been sent — and the backend may have
// computed its total — before the later turn completed, so its answer can
// predate the very turn that triggered the second call. Dropping it would
// leave the stored figure describing a state before the last settled turn,
// permanently, until some future turn happened to settle while the slot was
// free.
func TestCodexThreadCostReadIsSingleFlightWithARerun(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-claim"

	if _, ok := app.claimCodexThreadCostRead(id, "tok-1"); !ok {
		t.Fatal("the first claim was refused")
	}
	// Two settles land during the in-flight read.
	if _, ok := app.claimCodexThreadCostRead(id, "tok-2"); ok {
		t.Fatal("a second read was admitted while one was in flight")
	}
	if _, ok := app.claimCodexThreadCostRead(id, "tok-3"); ok {
		t.Fatal("a third read was admitted while one was in flight")
	}

	// B24(b): the rerun runs against the LATEST settle's session, not the
	// first claimant's. A rerun sent at a replaced session fails its own
	// liveness check and silently does nothing, so the newest turn would
	// never reach the backend.
	token, _, again := app.nextCodexThreadCostRead(id)
	if !again {
		t.Fatal("settles during the read armed no re-read — the stored figure would predate them")
	}
	if token != "tok-3" {
		t.Fatalf("re-read token = %q, want the latest settle's tok-3", token)
	}

	// A settle during the RE-read arms exactly one more pass; without one
	// the slot is released in the same critical section.
	if _, ok := app.claimCodexThreadCostRead(id, "tok-4"); ok {
		t.Fatal("a read was admitted while the re-read held the slot")
	}
	token, _, again = app.nextCodexThreadCostRead(id)
	if !again || token != "tok-4" {
		t.Fatalf("second re-read = (%q, %v), want (tok-4, true)", token, again)
	}

	if _, _, again := app.nextCodexThreadCostRead(id); again {
		t.Fatal("a clean slot asked for another pass — the re-read would loop")
	}
	if _, ok := app.claimCodexThreadCostRead(id, "tok-5"); !ok {
		t.Fatal("the slot was not released")
	}
	if _, _, again := app.nextCodexThreadCostRead(id); again {
		t.Fatal("a fresh slot started dirty")
	}

	// Released means gone, not zero-valued: a per-thread map that only ever
	// grew would be a leak on a long-lived app.
	app.costMu.Lock()
	_, present := app.costInflight[id]
	app.costMu.Unlock()
	if present {
		t.Fatal("the released slot left an entry behind")
	}
}

// B24(a), the lost wakeup. Ending a pass must check dirty and release the
// slot in ONE critical section. Split apart — read the flag, unlock, delete
// in a second section — a settle landing in between marks a slot that is
// about to be deleted, and its claimant has already been told "someone else
// will re-read for you": the stored figure then stays behind the last
// settled turn until some future turn happens to settle while the slot is
// free.
//
// The invariant is a strict pairing between the two sides of that race.
// Exactly one of them must come away owning the next read:
//
//   - the claim was REFUSED → it marked the slot dirty, so the owner's pass
//     must have been told to run again;
//   - the claim SUCCEEDED → it found a released slot and is the new owner,
//     so the previous owner must have been told to stop.
//
// Both true would mean two readers; both false is the lost wakeup — a
// settle that was promised a re-read nobody is going to perform.
func TestCodexThreadCostReleaseCannotLoseASettle(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-lost-wakeup"

	for round := range 2000 {
		if _, ok := app.claimCodexThreadCostRead(id, "tok-owner"); !ok {
			t.Fatalf("round %d: the owner could not claim a free slot", round)
		}
		var (
			wg          sync.WaitGroup
			claimed     bool
			anotherPass bool
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, claimed = app.claimCodexThreadCostRead(id, "tok-settle")
		}()
		go func() {
			defer wg.Done()
			_, _, anotherPass = app.nextCodexThreadCostRead(id)
		}()
		wg.Wait()

		if claimed == anotherPass {
			if claimed {
				t.Fatalf("round %d: a settle claimed the slot while the owner kept reading it", round)
			}
			t.Fatalf("round %d: a settle was refused (promised a re-read) and the owner released anyway — the wakeup is lost and the stored figure stays behind that turn", round)
		}
		// Drain whoever holds the slot so the next round starts clean.
		for {
			if _, _, again := app.nextCodexThreadCostRead(id); !again {
				break
			}
		}
	}
}

// B24(c): a rollback that repoints the thread's SessionRef must invalidate a
// read that is ALREADY out. Its answer is the old provider thread's total,
// and writing it back would restore exactly the row forgetCodexThreadCost
// just deleted.
func TestForgetCodexThreadCostFencesAnInFlightRead(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-fence"

	epoch, ok := app.claimCodexThreadCostRead(id, "tok-1")
	if !ok {
		t.Fatal("the claim was refused")
	}
	if !app.codexThreadCostReadIsCurrent(id, epoch) {
		t.Fatal("a freshly claimed read was already fenced")
	}

	app.forgetCodexThreadCost(id)

	if app.codexThreadCostReadIsCurrent(id, epoch) {
		t.Fatal("a read started before the rollback survived it — its answer would re-persist the deleted row")
	}
	// The NEXT pass is not fenced: it starts after the rollback and reads
	// the thread as it is now.
	app.costMu.Lock()
	app.costInflight[id].dirty = true
	app.costMu.Unlock()
	_, nextEpoch, again := app.nextCodexThreadCostRead(id)
	if !again {
		t.Fatal("the dirty slot refused another pass")
	}
	if !app.codexThreadCostReadIsCurrent(id, nextEpoch) {
		t.Fatal("a read started after the rollback was fenced by it")
	}
}

// The session-token guard. A session replaced between the settle and the read
// is a different process, possibly on a different login, and its estimate
// would be attributed to this thread wrongly — so a read whose token no
// longer matches persists nothing at all.
func TestCodexThreadCostReadRefusesAReplacedSession(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-token"
	thread := testCostThread(id)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// A live session under a DIFFERENT token than the settled turn's, and with
	// no codex handle — either alone must stop the read before any wire work is
	// attempted.
	app.sessions[id] = LiveSession{Token: "token-new"}

	app.readCodexThreadCost(id, "token-old", 0)

	if _, found, err := app.store.GetProviderThreadCost(id); err != nil {
		t.Fatalf("GetProviderThreadCost() error = %v", err)
	} else if found {
		t.Fatal("a read against a replaced session persisted a figure")
	}
}

// forgetCodexThreadCost is what keeps a repointed thread from showing another
// provider thread's total: the row is keyed by the AO thread but DESCRIBES
// the provider thread it was read from.
func TestForgetCodexThreadCostDropsTheStoredFigure(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-forget"
	if err := app.store.CreateThread(codexCostThread(id, "codex-thread-forget")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID:      id,
		SessionRef:    "codex-thread-forget",
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: 1_234_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost() error = %v", err)
	}

	app.forgetCodexThreadCost(id)

	if _, found, err := app.store.GetProviderThreadCost(id); err != nil {
		t.Fatalf("GetProviderThreadCost() error = %v", err)
	} else if found {
		t.Fatal("the stored figure survived the rollback that repointed the thread")
	}
	// Idempotent: a rollback on a thread that never had a figure is not an
	// error, and neither is a second one.
	app.forgetCodexThreadCost(id)
	app.forgetCodexThreadCost("thread-that-never-existed")
}

// A RESUMED provider thread has spend the backend knows about and no AO
// ledger rows at all — QueryUsage returns nothing for it. Reporting "no
// usage" beside a provider figure already on disk would hide the better
// answer, so the lifetime bucket is synthesized: zero tokens and zero turns
// (the truth about the ledger) with the provider's dollars.
func TestProviderThreadCostSynthesizesTheBucketForAResumedThread(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-resumed"
	if err := app.store.CreateThread(codexCostThread(id, "codex-thread-resumed")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID:      id,
		SessionRef:    "codex-thread-resumed",
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: 2_500_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost() error = %v", err)
	}

	buckets := app.overlayProviderThreadCost(store.UsageQuery{ThreadID: id}, nil)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want one synthesized lifetime bucket", len(buckets))
	}
	if got, want := buckets[0].CostUSD, 2.5; got != want {
		t.Fatalf("CostUSD = %v, want %v", got, want)
	}
	if buckets[0].CostSource != store.ProviderThreadCostSourceEstimate {
		t.Fatalf("CostSource = %q, want the provider's own label", buckets[0].CostSource)
	}
	if buckets[0].InputTokens != 0 || buckets[0].OutputTokens != 0 || buckets[0].TurnCount != 0 {
		t.Fatalf("synthesized bucket = %+v, want zero ledger tokens and turns", buckets[0])
	}
	if buckets[0].UnpricedRows != 0 {
		t.Fatalf("UnpricedRows = %d — a lower-bound marker over a complete figure", buckets[0].UnpricedRows)
	}
}

// The synthesis is scoped exactly like the replacement: a query the provider
// figure cannot answer keeps the rate-table arithmetic, and an EMPTY result
// for such a query stays empty rather than growing a bucket out of nowhere.
func TestProviderThreadCostSynthesizesNothingForOtherQueryShapes(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-shapes"
	if err := app.store.CreateThread(codexCostThread(id, "codex-thread-shapes")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID:      id,
		SessionRef:    "codex-thread-shapes",
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: 2_500_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost() error = %v", err)
	}

	for name, query := range map[string]store.UsageQuery{
		"grouped":        {ThreadID: id, GroupBy: "day"},
		"model-filter":   {ThreadID: id, Model: "gpt-5.4"},
		"time-bounded":   {ThreadID: id, FromMillis: 1},
		"other-thread":   {ThreadID: "thread-without-a-figure"},
		"cross-thread":   {},
		"wrong-provider": {ThreadID: id, Provider: string(provider.Claude)},
	} {
		if got := app.overlayProviderThreadCost(query, nil); len(got) != 0 {
			t.Errorf("%s: buckets = %+v, want none synthesized", name, got)
		}
	}
}

// B24(d), the resurrected row. The epoch fence and the WRITE must be one
// critical section. Split apart — check, return, write — a rollback landing in
// between bumps the fence and deletes the row after the check has already
// passed, and the write then restores exactly the figure the rollback removed:
// a cumulative total describing a Codex thread this AO thread no longer points
// at, with nothing guaranteed to correct it (a rollback to turn zero may never
// produce another turn).
//
// The invariant is on the OUTCOME rather than on the ordering, because both
// orderings are legal: whichever of the two runs first, a thread whose
// SessionRef was just repointed must not still carry a provider figure.
func TestRollbackDuringAThreadCostWriteCannotResurrectTheRow(t *testing.T) {
	app := newCostTestHarness(t)
	const id = "thread-cost-write-vs-rollback"
	if err := app.store.CreateThread(codexCostThread(id, "codex-thread-race")); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	for round := range 200 {
		epoch, ok := app.claimCodexThreadCostRead(id, "tok")
		if !ok {
			t.Fatalf("round %d: the claim was refused", round)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			app.forgetCodexThreadCost(id)
		}()
		go func() {
			defer wg.Done()
			if _, err := app.persistCodexThreadCostIfCurrent(id, epoch, store.ProviderThreadCost{
				ThreadID:      id,
				SessionRef:    "codex-thread-race",
				Provider:      string(provider.Codex),
				CostSource:    store.ProviderThreadCostSourceEstimate,
				CostUSDMicros: 137_500,
				UpdatedAt:     time.Now().UnixMilli(),
			}); err != nil {
				t.Errorf("round %d: persist: %v", round, err)
			}
		}()
		wg.Wait()

		if _, found, err := app.store.GetProviderThreadCost(id); err != nil {
			t.Fatalf("round %d: GetProviderThreadCost() error = %v", round, err)
		} else if found {
			t.Fatalf("round %d: a read that started before the rollback wrote its answer back — the row describes a Codex thread this thread no longer is", round)
		}
		// Release the slot so the next round starts from a free one.
		for {
			if _, _, again := app.nextCodexThreadCostRead(id); !again {
				break
			}
		}
	}
}

// L1: a rollback whose DELETE fails must not leave the old provider thread's
// total on screen, and that must not depend on this process staying alive.
//
// The row records WHICH provider thread it was read from (migration v68) and
// every read compares that against the thread's current SessionRef, so the
// rollback's repoint is what invalidates it — the delete is housekeeping. A
// row the delete could not remove is therefore unreadable by construction: the
// usage surfaces fall back to the rate table exactly as if it were gone, on
// this App and on any later one.
//
// The previous design hid the row behind an in-memory "forget failed" mark.
// That mark died with the process, so a restart served the old Codex thread's
// lifetime total against a thread that no longer points at it — and a rollback
// to turn zero may never produce another turn to overwrite it, so nothing was
// guaranteed to correct it. The restart leg below is the half that failed.
func TestFailedThreadCostDeleteIsHarmlessAcrossARestart(t *testing.T) {
	app, dbPath := newCostTestHarnessWithPath(t)
	const id = "thread-cost-forget-fails"
	thread := codexCostThread(id, "codex-thread-before")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.PutProviderThreadCost(store.ProviderThreadCost{
		ThreadID:      id,
		SessionRef:    "codex-thread-before",
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: 2_500_000,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PutProviderThreadCost() error = %v", err)
	}
	if got := app.overlayProviderThreadCost(store.UsageQuery{ThreadID: id}, nil); len(got) != 1 || got[0].CostUSD != 2.5 {
		t.Fatalf("baseline overlay = %+v, want the stored provider figure", got)
	}

	// The rollback forks onto a new Codex thread and persists that first —
	// the order app_conversation_rollback.go uses, because a failed
	// UpdateThread must leave the thread on the ref the stored figure still
	// correctly describes.
	thread.SessionRef = "codex-thread-forked"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	// A BEFORE DELETE trigger is the one injection that reproduces the shape
	// that matters: the store's delete fails while its reads keep working, so
	// the stale row stays physically present and perfectly readable.
	guard, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open guard handle: %v", err)
	}
	t.Cleanup(func() { _ = guard.Close() })
	if _, err := guard.Exec(`CREATE TRIGGER refuse_provider_thread_cost_delete
		BEFORE DELETE ON provider_thread_cost
		BEGIN SELECT RAISE(ABORT, 'delete refused by test'); END`); err != nil {
		t.Fatalf("install delete trigger: %v", err)
	}

	app.forgetCodexThreadCost(id)

	var surviving int
	if err := guard.QueryRow(
		`SELECT COUNT(*) FROM provider_thread_cost WHERE thread_id = ?`, id,
	).Scan(&surviving); err != nil {
		t.Fatalf("count surviving rows: %v", err)
	}
	if surviving != 1 {
		t.Fatalf("the trigger did not keep the row on disk (%d rows) — the test proves nothing", surviving)
	}
	if _, found, err := app.store.GetProviderThreadCost(id); err != nil || found {
		t.Fatalf("a row naming the OLD Codex thread was served (found %v, err %v)", found, err)
	}
	if got := app.overlayProviderThreadCost(store.UsageQuery{ThreadID: id}, nil); len(got) != 0 {
		t.Fatalf("overlay = %+v, want the rate-table answer: a rollback whose delete failed still showed the old Codex thread's total", got)
	}

	// The restart. A second App over the same store holds none of the first
	// one's memory, which is exactly what the old forget-failed mark could not
	// survive. Nothing here has re-attempted the delete, so the row is still
	// on disk.
	restarted := New(Deps{Store: app.store})
	if got := restarted.overlayProviderThreadCost(store.UsageQuery{ThreadID: id}, nil); len(got) != 0 {
		t.Fatalf("overlay after a restart = %+v — the invalidation died with the process and the old Codex thread's total came back", got)
	}

	// Self-healing: the next settled turn prices the thread the rollback moved
	// it to, rewriting the same row under the new identity.
	epoch, ok := app.claimCodexThreadCostRead(id, "tok")
	if !ok {
		t.Fatal("the claim was refused")
	}
	stored, err := app.persistCodexThreadCostIfCurrent(id, epoch, store.ProviderThreadCost{
		ThreadID:      id,
		SessionRef:    "codex-thread-forked",
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: 3_000_000,
		UpdatedAt:     time.Now().UnixMilli(),
	})
	if err != nil || !stored {
		t.Fatalf("re-read persist = (%v, %v), want a stored figure", stored, err)
	}
	if got := app.overlayProviderThreadCost(store.UsageQuery{ThreadID: id}, nil); len(got) != 1 || got[0].CostUSD != 3 {
		t.Fatalf("overlay after the re-read = %+v, want the fresh provider figure", got)
	}
}
