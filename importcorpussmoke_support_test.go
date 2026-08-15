//go:build !providersmoke

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/provider/codex/rollout"
	importwriter "agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
)

// importcorpussmoke_support_test.go — the two provider legs of the
// import-corpus gate, the throwaway writer they Build against, and the report
// they fill in. The gate itself — the env contract, the live-home refusal,
// and the corpus-shape resolution — lives in importcorpussmoke_test.go, which
// is also where the whole thing is documented.

// --- the Claude leg ---

func scanClaudeCorpus(t *testing.T, projectsDir string) *corpusReport {
	t.Helper()
	report := newCorpusReport(string(provider.Claude), projectsDir)
	writer := newCorpusWriter(t, string(provider.Claude))

	sessions, warnings, err := claudesessions.List(t.Context(), claudesessions.Options{ProjectsDir: projectsDir})
	if err != nil {
		t.Fatalf("import corpus (claude): list %s: %v", projectsDir, err)
	}
	report.countWarnings(warnings)
	report.listed = len(sessions)
	if len(sessions) == 0 {
		t.Fatalf("import corpus (claude): %s listed no importable sessions — that is a corpus problem, not a result", projectsDir)
	}
	expectedParents := make(map[string]string, len(sessions))
	for _, session := range sessions {
		expectedParents[session.SessionID] = session.ForkedFromSessionID
	}
	scanParents := report.verifyProductionScan(t, importwriter.Deps{
		Store: writer.store, ClaudeProjectsDir: projectsDir,
	}, expectedParents)

	for _, session := range sessions {
		report.bytes += session.SizeBytes
		report.session(func() { scanOneClaudeSession(t, writer, report, session) })
	}
	report.verifyImportedLineage(t, writer, scanParents)
	return report
}

func scanOneClaudeSession(
	t *testing.T, writer *corpusWriter, report *corpusReport, session claudesessions.SessionInfo,
) {
	t.Helper()
	eventsBefore, rowsBefore := report.events, report.rows
	writer.beginSession()
	defer func() {
		started := time.Now()
		if err := writer.endSession(); err != nil {
			report.fail(t, session.Path, fmt.Errorf("clean applied session: %w", err))
		}
		report.cleanupDuration += time.Since(started)
	}()
	started := time.Now()
	loaded, err := claudesessions.LoadSession(session.Path)
	report.providerReadDuration += time.Since(started)
	if err != nil {
		if errors.Is(err, claudesessions.ErrTranscriptTooLarge) {
			// The documented per-session refusal (transcript.go: 1 GB on the
			// stat, before a byte is read). It is a designed outcome, so it
			// is counted and reported rather than failed.
			report.refused++
			return
		}
		report.fail(t, session.Path, fmt.Errorf("load: %w", err))
		return
	}
	defer loaded.Close()

	report.countWarnings(loaded.Warnings)
	report.branches += len(loaded.Branches)
	started = time.Now()
	branch, found, err := loaded.ConvertActiveBranch()
	report.convertDuration += time.Since(started)
	if err != nil {
		report.fail(t, session.Path, fmt.Errorf("convert active branch: %w", err))
		return
	}
	if found {
		report.countWarnings(branch.Warnings)
		if len(branch.Events) > 0 {
			profileSource := "transcript"
			if branch.Profile.Model == "" {
				profileSource = "provider-default"
			}
			report.observeProfile(branch.Profile, profileSource)
			if _, ok := report.build(
				t, writer, session.Path, "active branch", branch.Events, branch.Profile,
			); !ok {
				return
			}
		}
	}
	report.observeSession(session.Path, session.SizeBytes, len(loaded.Branches),
		report.events-eventsBefore, report.rows-rowsBefore)
	report.ok++
}

// --- the Codex leg ---

func scanCodexCorpus(t *testing.T, codexHome string) *corpusReport {
	t.Helper()
	report := newCorpusReport(string(provider.Codex), codexHome)
	writer := newCorpusWriter(t, string(provider.Codex))

	sessions, warnings, err := rollout.List(t.Context(), rollout.ListOptions{CodexHome: codexHome})
	if err != nil {
		t.Fatalf("import corpus (codex): list %s: %v", codexHome, err)
	}
	report.countWarnings(warnings)
	report.listed = len(sessions)
	if len(sessions) == 0 {
		t.Fatalf("import corpus (codex): %s listed no importable sessions.%s",
			codexHome, codexCorpusDiagnosis(codexHome, report))
	}
	expectedSessions := make(map[string]string, len(sessions))
	for _, session := range sessions {
		// Codex keeps explicit-fork provenance in the rollout rather than its
		// state index. An empty expected value asks verifyProductionScan to
		// prove membership now; scan-vs-rollout provenance is checked after
		// Parse below.
		expectedSessions[session.ThreadID] = ""
	}
	scanParents := report.verifyProductionScan(t, importwriter.Deps{
		Store: writer.store, CodexHome: codexHome,
	}, expectedSessions)

	for _, session := range sessions {
		report.bytes += session.SizeBytes
		report.session(func() {
			scanOneCodexSession(t, writer, report, session, scanParents[session.ThreadID])
		})
	}
	report.verifyImportedLineage(t, writer, scanParents)
	return report
}

// codexCorpusDiagnosis names the one corpus-preparation mistake that produces
// an empty listing from a complete copy: `threads.rollout_path` is absolute,
// so an un-rewritten copy points every row back at the live home and
// `rollout.PathInHome` — correctly — refuses all of them.
func codexCorpusDiagnosis(codexHome string, report *corpusReport) string {
	if report.warnings[rollout.WarnRolloutOutside] == 0 {
		return ""
	}
	return fmt.Sprintf(
		"\nEvery row (%d) records a rollout file outside %s, which means the copy's thread index still points at the ORIGINAL home."+
			"\nRepoint it (on the COPY, never the original):"+
			"\n  sqlite3 %s \"UPDATE threads SET rollout_path = replace(rollout_path, '<original ~/.codex>', '%s');\"",
		report.warnings[rollout.WarnRolloutOutside], codexHome,
		filepath.Join(codexHome, rollout.StateDBName), codexHome)
}

func scanOneCodexSession(
	t *testing.T, writer *corpusWriter, report *corpusReport, session rollout.SessionInfo,
	scannedParentSessionID string,
) {
	t.Helper()
	eventsBefore, rowsBefore := report.events, report.rows
	writer.beginSession()
	defer func() {
		started := time.Now()
		if err := writer.endSession(); err != nil {
			report.fail(t, session.RolloutPath, fmt.Errorf("clean applied session: %w", err))
		}
		report.cleanupDuration += time.Since(started)
	}()
	// Same call production makes (importCodex): the thread id is the
	// authority for which session_meta line belongs to this file, and a fork
	// embeds its source's meta too.
	started := time.Now()
	parsed, err := rollout.Parse(t.Context(), rollout.ParseOptions{
		Path:      session.RolloutPath,
		SessionID: session.ThreadID,
	})
	report.providerReadDuration += time.Since(started)
	if err != nil {
		report.fail(t, session.RolloutPath, fmt.Errorf("parse: %w", err))
		return
	}
	if parsed.Meta.ForkedFromID != scannedParentSessionID {
		report.fail(t, session.RolloutPath, fmt.Errorf(
			"production scan recorded fork parent %q, full rollout parse recorded %q",
			scannedParentSessionID, parsed.Meta.ForkedFromID))
		return
	}
	report.countWarnings(parsed.Warnings)
	report.corruptLines += parsed.CorruptLines
	for name, count := range parsed.UnknownTypes {
		report.unknownTypes[name] += count
	}
	profile := parsed.Profile
	profileSource := "rollout"
	if profile.Model == "" {
		profile.Model = session.Model
		profile.ReasoningEffort = session.ReasoningEffort
		profileSource = "index-fallback"
	} else if profile.Model == session.Model && profile.ReasoningEffort == "" {
		profile.ReasoningEffort = session.ReasoningEffort
	}
	if profile.Model == "" {
		profileSource = "provider-default"
	}
	report.observeProfile(profile, profileSource)
	if _, ok := report.build(t, writer, session.RolloutPath, "rollout", parsed.Events, profile); !ok {
		return
	}
	report.observeSession(session.RolloutPath, session.SizeBytes, 1,
		report.events-eventsBefore, report.rows-rowsBefore)
	report.ok++
}

// --- the throwaway writer ---

// corpusWriter owns the throwaway store used by the gate. Each provider
// session gets a fresh thread, then Build and ApplyImportBatch run exactly as
// they do in production. endSession deletes it so the file-backed store stays
// bounded to one source session.
type corpusWriter struct {
	store          *store.Store
	project        store.Project
	provider       string
	nextThread     int
	sessionThreads []string
}

func newCorpusWriter(t *testing.T, providerName string) *corpusWriter {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(filepath.Join(root, "import-corpus.sqlite"))
	if err != nil {
		t.Fatalf("import corpus (%s): open throwaway store: %v", providerName, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const seedMillis = int64(1)
	project := store.Project{
		ID:        "import-corpus-smoke",
		Path:      root,
		Name:      "Import corpus smoke",
		CreatedAt: seedMillis,
		UpdatedAt: seedMillis,
	}
	if err := st.CreateProject(project); err != nil {
		t.Fatalf("import corpus (%s): seed project: %v", providerName, err)
	}
	return &corpusWriter{store: st, project: project, provider: providerName}
}

func (w *corpusWriter) beginSession() {
	w.sessionThreads = w.sessionThreads[:0]
}

func (w *corpusWriter) endSession() error {
	for _, threadID := range w.sessionThreads {
		if err := w.store.RollbackImportedThread(threadID); err != nil {
			return fmt.Errorf("delete thread %s: %w", threadID, err)
		}
	}
	w.sessionThreads = w.sessionThreads[:0]
	return nil
}

func (w *corpusWriter) buildAndApply(
	events []importir.Event, profile importir.ModelProfile,
) (store.ImportBatch, []importir.Warning, string, time.Duration, time.Duration, error) {
	if len(w.sessionThreads) != 0 {
		return store.ImportBatch{}, nil, "", 0, 0,
			fmt.Errorf("one provider session attempted to materialize more than one thread")
	}
	w.nextThread++
	thread := store.Thread{
		ID:              fmt.Sprintf("import-corpus-%s-%d", w.provider, w.nextThread),
		ProjectID:       w.project.ID,
		Title:           "Import corpus smoke",
		Provider:        w.provider,
		WorkspacePath:   w.project.Path,
		CreatedAt:       1,
		UpdatedAt:       1,
		ImportSource:    w.provider,
		Model:           profile.Model,
		ReasoningEffort: profile.ReasoningEffort,
		ContextWindow:   profile.ContextWindow,
	}
	thread = chatmodel.SanitizeThread(thread)
	if err := w.store.CreateThread(thread); err != nil {
		return store.ImportBatch{}, nil, "", 0, 0, fmt.Errorf("create throwaway thread: %w", err)
	}
	w.sessionThreads = append(w.sessionThreads, thread.ID)

	applyDuration := time.Duration(0)
	started := time.Now()
	batch, warnings, err := importwriter.NewWriter(w.store, thread).Build(events)
	buildDuration := time.Since(started)
	if err != nil {
		return store.ImportBatch{}, warnings, thread.ID, buildDuration, applyDuration, err
	}
	started = time.Now()
	if err := w.store.ApplyImportBatch(thread.ID, batch); err != nil {
		applyDuration += time.Since(started)
		return store.ImportBatch{}, warnings, thread.ID,
			buildDuration, applyDuration, fmt.Errorf("apply import batch: %w", err)
	}
	applyDuration += time.Since(started)
	return batch, warnings, thread.ID, buildDuration, applyDuration, nil
}

// --- the report ---

// corpusReport accumulates one provider leg. Everything on it is a count or a
// histogram: the gate's output has to stay readable for a corpus of a
// thousand sessions, so nothing here retains a session's content.
type corpusReport struct {
	provider string
	root     string
	started  time.Time

	listed   int
	ok       int
	refused  int
	branches int
	forks    int
	// lineageResolved is the number of explicit provider-fork edges whose
	// parent is present in this corpus and was mapped to the exact AO thread.
	// lineageUnresolved counts honest dangling provenance: the provider names
	// a parent session whose source is absent from the copied corpus.
	lineageResolved   int
	lineageUnresolved int
	lineageUnsafe     int
	threads           int
	events            int
	rows              int
	bytes             int64

	corruptLines   int
	unknownTypes   map[string]int
	warnings       map[string]int
	profileSources map[string]int
	profileModels  map[string]int
	// warningTotal is the running sum of the histogram, and warnedSessions
	// counts sessions that contributed at least one. The two answer
	// different questions — "how much drift" and "how widespread" — and one
	// noisy session must not read as a corpus-wide problem.
	warningTotal   int
	warnedSessions int

	failures     int
	peakHeapByte uint64

	providerReadDuration   time.Duration
	productionScanDuration time.Duration
	lineageDuration        time.Duration
	convertDuration        time.Duration
	buildDuration          time.Duration
	applyDuration          time.Duration
	cleanupDuration        time.Duration

	mostBranches corpusSessionMaterialization
	mostRows     corpusSessionMaterialization
}

// verifyProductionScan proves the real orchestrator offers every top-level
// session the provider lister accepted. This is deliberately in addition to
// the reader/writer loop: the historical fork bug lived only in Scan's
// candidate subtraction, so parsing and applying every file directly could
// report a clean corpus while Import All silently omitted ancestors.
//
// The return value is the parent id production attached to each row. For
// Claude, expectedParents also carries the lister's parent and is compared
// here. Codex's index has no parent column, so its full Parse compares the
// returned value to authoritative rollout metadata later.
func (r *corpusReport) verifyProductionScan(
	t *testing.T,
	deps importwriter.Deps,
	expectedParents map[string]string,
) map[string]string {
	t.Helper()
	started := time.Now()
	result, err := importwriter.Scan(t.Context(), deps, importwriter.Filter{Provider: r.provider})
	r.productionScanDuration += time.Since(started)
	if err != nil {
		t.Fatalf("import corpus (%s): production scan: %v", r.provider, err)
	}
	if len(result.Providers) != 1 || !result.Providers[0].Available {
		t.Fatalf("import corpus (%s): production provider status = %+v", r.provider, result.Providers)
	}
	parents := make(map[string]string, len(result.Rows))
	for _, row := range result.Rows {
		if row.Provider != r.provider {
			t.Fatalf("import corpus (%s): production scan returned foreign row %s", r.provider, row.ID)
		}
		if _, duplicate := parents[row.SessionID]; duplicate {
			t.Fatalf("import corpus (%s): production scan returned session %s twice", r.provider, row.SessionID)
		}
		parents[row.SessionID] = row.ParentSessionID
		if row.ParentSessionID != "" {
			r.forks++
		}
	}
	if len(parents) != len(expectedParents) {
		t.Fatalf("import corpus (%s): production scan returned %d of %d listed top-level sessions",
			r.provider, len(parents), len(expectedParents))
	}
	for sessionID, expectedParent := range expectedParents {
		gotParent, found := parents[sessionID]
		if !found {
			t.Fatalf("IMPORT CORPUS FAILURE (%s): production scan suppressed listed session %s",
				r.provider, sessionID)
		}
		if r.provider == string(provider.Claude) && gotParent != expectedParent {
			t.Errorf("IMPORT CORPUS FAILURE (%s): session %s parent = %q, lister recorded %q",
				r.provider, sessionID, gotParent, expectedParent)
		}
	}
	return parents
}

// verifyImportedLineage materializes the corpus's real provider-fork graph in
// the throwaway store. The per-session loop above proves each history in
// isolation; this pass proves those independently valid histories are joined
// without collapsing a session, inventing a parent, or depending on import
// order. Histories are not replayed a second time, so the cost is proportional
// to the small thread/import-state rows rather than corpus bytes.
func (r *corpusReport) verifyImportedLineage(
	t *testing.T,
	writer *corpusWriter,
	parents map[string]string,
) {
	t.Helper()
	started := time.Now()
	defer func() { r.lineageDuration += time.Since(started) }()

	sessionIDs := make([]string, 0, len(parents))
	for sessionID := range parents {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	threadBySession := make(map[string]string, len(sessionIDs))
	created := make([]string, 0, len(sessionIDs))
	for i, sessionID := range sessionIDs {
		threadID := fmt.Sprintf("import-corpus-lineage-%s-%d", r.provider, i+1)
		thread := store.Thread{
			ID:            threadID,
			ProjectID:     writer.project.ID,
			Title:         "Import corpus lineage",
			Provider:      r.provider,
			WorkspacePath: writer.project.Path,
			SessionRef:    sessionID,
			CreatedAt:     1,
			UpdatedAt:     1,
			ImportSource:  r.provider,
		}
		if err := writer.store.CreateThread(thread); err != nil {
			t.Fatalf("import corpus (%s): create lineage thread for %s: %v", r.provider, sessionID, err)
		}
		created = append(created, threadID)
		threadBySession[sessionID] = threadID
		if err := writer.store.SetThreadImportState(store.ThreadImportState{
			ThreadID:              threadID,
			Provider:              r.provider,
			SourceSessionID:       sessionID,
			SourceParentSessionID: parents[sessionID],
			LastTurnIndex:         -1,
			LastItemIndex:         -1,
			ImportedAt:            1,
		}); err != nil {
			t.Fatalf("import corpus (%s): record lineage for %s: %v", r.provider, sessionID, err)
		}
	}

	unsafe := make(map[string]bool)
	seenWarnings := make(map[string]bool)
	for _, sessionID := range sessionIDs {
		warnings, err := writer.store.ReconcileImportedForkLineage(r.provider, sessionID)
		if err != nil {
			t.Fatalf("import corpus (%s): reconcile lineage for %s: %v", r.provider, sessionID, err)
		}
		for _, warning := range warnings {
			unsafe[warning.ThreadID] = true
			key := warning.ThreadID + "\x00" + warning.Code
			if !seenWarnings[key] {
				seenWarnings[key] = true
				r.warnings[warning.Code]++
				r.warningTotal++
				r.lineageUnsafe++
			}
		}
	}

	for _, sessionID := range sessionIDs {
		threadID := threadBySession[sessionID]
		thread, err := writer.store.GetThread(threadID)
		if err != nil {
			t.Fatalf("import corpus (%s): read lineage thread for %s: %v", r.provider, sessionID, err)
		}
		parentSessionID := parents[sessionID]
		expectedParent := ""
		if !unsafe[threadID] && parentSessionID != "" {
			if resolved, found := threadBySession[parentSessionID]; found {
				expectedParent = resolved
				r.lineageResolved++
			} else {
				r.lineageUnresolved++
			}
		}
		if thread.ForkedFromThreadID != expectedParent {
			t.Fatalf(
				"IMPORT CORPUS FAILURE (%s): session %s resolved parent thread %q, want %q for provider parent %q",
				r.provider, sessionID, thread.ForkedFromThreadID, expectedParent, parentSessionID)
		}
	}

	cleanupStarted := time.Now()
	for _, threadID := range created {
		if err := writer.store.RollbackImportedThread(threadID); err != nil {
			t.Fatalf("import corpus (%s): clean lineage thread %s: %v", r.provider, threadID, err)
		}
	}
	r.cleanupDuration += time.Since(cleanupStarted)
}

// corpusSessionMaterialization explains why a small-looking source corpus can
// still be expensive to import. Claude stores a branch DAG once and can
// retain many leaves, while AO materializes only its active history; the
// active event and row counts therefore matter at least as much as source
// file size.
type corpusSessionMaterialization struct {
	path        string
	sourceBytes int64
	branches    int
	events      int
	rows        int
}

func newCorpusReport(providerName, root string) *corpusReport {
	return &corpusReport{
		provider:       providerName,
		root:           root,
		started:        time.Now(),
		unknownTypes:   map[string]int{},
		warnings:       map[string]int{},
		profileSources: map[string]int{},
		profileModels:  map[string]int{},
	}
}

func (r *corpusReport) observeProfile(profile importir.ModelProfile, source string) {
	r.profileSources[source]++
	if profile.Model != "" {
		r.profileModels[profile.Model]++
	}
}

func (r *corpusReport) countWarnings(warnings []importir.Warning) {
	for _, warning := range warnings {
		code := warning.Code
		if code == "" {
			code = "(uncoded)"
		}
		r.warnings[code]++
		r.warningTotal++
	}
}

func (r *corpusReport) observeSession(path string, sourceBytes int64, branches, events, rows int) {
	observation := corpusSessionMaterialization{
		path:        path,
		sourceBytes: sourceBytes,
		branches:    branches,
		events:      events,
		rows:        rows,
	}
	if branches > r.mostBranches.branches {
		r.mostBranches = observation
	}
	if rows > r.mostRows.rows {
		r.mostRows = observation
	}
}

// session brackets one session's work so the report can attribute warnings to
// the session that raised them. The callers pass the whole per-session body,
// which is also what guarantees every allocation it made — the skeleton, one
// branch's events, one batch of rows — is unreachable before the next session
// is opened.
func (r *corpusReport) session(body func()) {
	before := r.warningTotal
	body()
	if r.warningTotal > before {
		r.warnedSessions++
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > r.peakHeapByte {
		r.peakHeapByte = stats.HeapAlloc
	}
}

// build runs the writer and transactional store apply over one unit of events,
// then records what the build produced. It returns false on either failure so
// the caller stops working on a session whose contract is already broken.
func (r *corpusReport) build(
	t *testing.T,
	writer *corpusWriter,
	path, unit string,
	events []importir.Event,
	profile importir.ModelProfile,
) (string, bool) {
	t.Helper()
	r.events += len(events)
	batch, warnings, threadID, buildDuration, applyDuration, err :=
		writer.buildAndApply(events, profile)
	r.buildDuration += buildDuration
	r.applyDuration += applyDuration
	if err != nil {
		r.fail(t, path, fmt.Errorf("build/apply %s: %w", unit, err))
		return "", false
	}
	r.countWarnings(warnings)
	r.threads++
	r.rows += len(batch.Rows)
	return threadID, true
}

// fail records one hard error. The first corpusFailureLimit are printed in
// full; the rest are counted, because a systemic reader defect otherwise
// buries its own count under thousands of identical lines.
func (r *corpusReport) fail(t *testing.T, path string, err error) {
	t.Helper()
	r.failures++
	if r.failures <= corpusFailureLimit {
		t.Errorf("IMPORT CORPUS FAILURE (%s): %s: %v", r.provider, path, err)
	}
}

// log prints the summary tables. This is the gate's actual product: format
// drift surfaces as a NEW warning code or a NEW unknown type between runs,
// which is only visible if every run prints the whole set.
func (r *corpusReport) log(t *testing.T) {
	t.Helper()
	elapsed := time.Since(r.started)
	t.Logf("import corpus (%s): root=%s", r.provider, r.root)
	t.Logf("import corpus (%s): sessions listed=%d ok=%d warned=%d refused-too-large=%d failed=%d",
		r.provider, r.listed, r.ok, r.warnedSessions, r.refused, r.failures)
	if r.branches > 0 {
		t.Logf("import corpus (%s): branches=%d", r.provider, r.branches)
	}
	t.Logf("import corpus (%s): explicit-provider-forks=%d", r.provider, r.forks)
	t.Logf("import corpus (%s): fork-lineage resolved=%d unresolved-parent-source=%d unsafe-edges-omitted=%d",
		r.provider, r.lineageResolved, r.lineageUnresolved, r.lineageUnsafe)
	t.Logf("import corpus (%s): materialized-threads=%d", r.provider, r.threads)
	t.Logf("import corpus (%s): converted-events=%d logical-rows=%d source=%s",
		r.provider, r.events, r.rows, humanBytes(r.bytes))
	if r.mostBranches.branches > 1 {
		r.logMaterialization(t, "most branches", r.mostBranches)
	}
	if r.mostRows.rows > 0 && r.mostRows.path != r.mostBranches.path {
		r.logMaterialization(t, "most rows", r.mostRows)
	}
	if r.corruptLines > 0 {
		t.Logf("import corpus (%s): corrupt/oversized lines skipped=%d", r.provider, r.corruptLines)
	}
	logHistogram(t, r.provider, "warnings by code", r.warnings)
	logHistogram(t, r.provider, "unknown wire types", r.unknownTypes)
	logHistogram(t, r.provider, "model profiles by source", r.profileSources)
	logHistogram(t, r.provider, "recorded models", r.profileModels)
	t.Logf("import corpus (%s): stage wall: production-scan=%s provider-read=%s branch-convert=%s writer-build=%s store-apply=%s lineage=%s cleanup=%s",
		r.provider,
		r.productionScanDuration.Round(time.Millisecond),
		r.providerReadDuration.Round(time.Millisecond),
		r.convertDuration.Round(time.Millisecond),
		r.buildDuration.Round(time.Millisecond),
		r.applyDuration.Round(time.Millisecond),
		r.lineageDuration.Round(time.Millisecond),
		r.cleanupDuration.Round(time.Millisecond))
	t.Logf("import corpus (%s): wall=%s peak-heap=%s",
		r.provider, elapsed.Round(time.Millisecond), humanBytes(int64(r.peakHeapByte)))
}

func (r *corpusReport) logMaterialization(
	t *testing.T, label string, materialization corpusSessionMaterialization,
) {
	t.Helper()
	path := materialization.path
	if relative, err := filepath.Rel(r.root, path); err == nil {
		path = relative
	}
	t.Logf("import corpus (%s): %s in one source session: branches=%d events=%d rows=%d source=%s path=%s",
		r.provider, label, materialization.branches, materialization.events,
		materialization.rows, humanBytes(materialization.sourceBytes), path)
}

// settle turns the collected failures into the gate's verdict. Failures are
// already reported individually; this is the line that says how many there
// were in total, including the ones the print limit swallowed.
func (r *corpusReport) settle(t *testing.T) {
	t.Helper()
	if r.failures == 0 {
		return
	}
	t.Fatalf(
		"IMPORT CORPUS FAILED (%s): %d of %d sessions could not be read, converted, built, or applied (%d printed above)",
		r.provider, r.failures, r.listed, min(r.failures, corpusFailureLimit))
}

func logHistogram(t *testing.T, providerName, label string, counts map[string]int) {
	t.Helper()
	if len(counts) == 0 {
		t.Logf("import corpus (%s): %s: none", providerName, label)
		return
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	// Descending by count, then by name: the drift worth reading is at the
	// top, and the tie-break keeps two runs of the same corpus comparable.
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	t.Logf("import corpus (%s): %s (%d):", providerName, label, len(keys))
	for _, key := range keys {
		t.Logf("import corpus (%s):   %8d  %s", providerName, counts[key], key)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
