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

	for _, session := range sessions {
		report.bytes += session.SizeBytes
		report.session(func() { scanOneClaudeSession(t, writer, report, session) })
	}
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
	donorThreads := make(map[int]string, len(loaded.Branches))
	for i := range loaded.Branches {
		started := time.Now()
		prefix, hasPrefix, err := loaded.FindReusablePrefix(i)
		if err != nil {
			report.convertDuration += time.Since(started)
			report.fail(t, session.Path, fmt.Errorf("find reusable prefix for branch %d/%d: %w", i, len(loaded.Branches), err))
			return
		}
		startRow := 0
		if hasPrefix {
			startRow = prefix.SuffixRow
		}
		branch, err := loaded.ConvertBranchFrom(i, startRow)
		report.convertDuration += time.Since(started)
		if err != nil {
			report.fail(t, session.Path, fmt.Errorf("convert branch %d/%d: %w", i, len(loaded.Branches), err))
			return
		}
		report.countWarnings(branch.Warnings)
		if len(branch.Events) == 0 && !hasPrefix {
			continue
		}
		prefixThreadID, prefixBeforeTurn := "", 0
		if hasPrefix {
			prefixThreadID = donorThreads[prefix.DonorIndex]
			prefixBeforeTurn = prefix.NextTurnIndex
		}
		threadID, ok := report.build(
			t, writer, session.Path, fmt.Sprintf("branch %d", i), branch.Events,
			prefixThreadID, prefixBeforeTurn,
		)
		if !ok {
			return
		}
		if err := loaded.AddReusablePrefixDonor(i); err != nil {
			report.fail(t, session.Path, fmt.Errorf("register prefix donor branch %d/%d: %w", i, len(loaded.Branches), err))
			return
		}
		donorThreads[i] = threadID
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

	for _, session := range sessions {
		report.bytes += session.SizeBytes
		report.session(func() { scanOneCodexSession(t, writer, report, session) })
	}
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
	report.countWarnings(parsed.Warnings)
	report.corruptLines += parsed.CorruptLines
	for name, count := range parsed.UnknownTypes {
		report.unknownTypes[name] += count
	}
	if _, ok := report.build(t, writer, session.RolloutPath, "rollout", parsed.Events, "", 0); !ok {
		return
	}
	report.observeSession(session.RolloutPath, session.SizeBytes, 1,
		report.events-eventsBefore, report.rows-rowsBefore)
	report.ok++
}

// --- the throwaway writer ---

// corpusWriter owns the throwaway store used by the gate. Each converted unit
// gets a fresh thread, then Build and ApplyImportBatch run exactly as they do
// in production. Claude branch threads remain alive until the whole source
// session has been applied, which is essential: shared-prefix payload IDs can
// only collide while sibling branches coexist. endSession deletes those
// threads so the file-backed store stays bounded to one source session.
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
	events []importir.Event, prefixSourceThreadID string, prefixBeforeTurn int,
) (store.ImportBatch, []importir.Warning, string, int, time.Duration, time.Duration, error) {
	w.nextThread++
	thread := store.Thread{
		ID:            fmt.Sprintf("import-corpus-%s-%d", w.provider, w.nextThread),
		ProjectID:     w.project.ID,
		Title:         "Import corpus smoke",
		Provider:      w.provider,
		WorkspacePath: w.project.Path,
		CreatedAt:     1,
		UpdatedAt:     1,
		ImportSource:  w.provider,
	}
	if err := w.store.CreateThread(thread); err != nil {
		return store.ImportBatch{}, nil, "", 0, 0, 0, fmt.Errorf("create throwaway thread: %w", err)
	}
	w.sessionThreads = append(w.sessionThreads, thread.ID)

	logicalPrefixRows := 0
	applyDuration := time.Duration(0)
	if prefixSourceThreadID != "" {
		boundaryAt := int64(0)
		for _, event := range events {
			if event.Kind == provider.EventTurnStart {
				boundaryAt = event.Timestamp.UnixMilli()
				break
			}
		}
		if boundaryAt <= 0 {
			return store.ImportBatch{}, nil, thread.ID, 0, 0, 0,
				fmt.Errorf("shared prefix has no suffix turn boundary")
		}
		started := time.Now()
		prefix, err := w.store.CloneImportedHistoryPrefix(
			prefixSourceThreadID, thread.ID, prefixBeforeTurn, boundaryAt)
		applyDuration += time.Since(started)
		if err != nil {
			return store.ImportBatch{}, nil, thread.ID, 0, 0, applyDuration,
				fmt.Errorf("clone imported prefix: %w", err)
		}
		logicalPrefixRows = prefix.ItemCount
	}

	started := time.Now()
	batch, warnings, err := importwriter.NewWriter(w.store, thread).Build(events)
	buildDuration := time.Since(started)
	if err != nil {
		return store.ImportBatch{}, warnings, thread.ID, logicalPrefixRows,
			buildDuration, applyDuration, err
	}
	started = time.Now()
	if err := w.store.ApplyImportBatch(thread.ID, batch); err != nil {
		applyDuration += time.Since(started)
		return store.ImportBatch{}, warnings, thread.ID, logicalPrefixRows,
			buildDuration, applyDuration, fmt.Errorf("apply import batch: %w", err)
	}
	applyDuration += time.Since(started)
	return batch, warnings, thread.ID, logicalPrefixRows + len(batch.Rows),
		buildDuration, applyDuration, nil
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
	events   int
	rows     int
	bytes    int64

	corruptLines int
	unknownTypes map[string]int
	warnings     map[string]int
	// warningTotal is the running sum of the histogram, and warnedSessions
	// counts sessions that contributed at least one. The two answer
	// different questions — "how much drift" and "how widespread" — and one
	// noisy session must not read as a corpus-wide problem.
	warningTotal   int
	warnedSessions int

	failures     int
	peakHeapByte uint64

	providerReadDuration time.Duration
	convertDuration      time.Duration
	buildDuration        time.Duration
	applyDuration        time.Duration
	cleanupDuration      time.Duration

	mostBranches corpusSessionMaterialization
	mostRows     corpusSessionMaterialization
}

// corpusSessionMaterialization explains why a small-looking source corpus can
// still be expensive to import. Claude stores a branch DAG once, while AO
// materializes one complete thread per leaf; the materialized event and row
// counts therefore matter at least as much as the source file's byte size.
type corpusSessionMaterialization struct {
	path        string
	sourceBytes int64
	branches    int
	events      int
	rows        int
}

func newCorpusReport(providerName, root string) *corpusReport {
	return &corpusReport{
		provider:     providerName,
		root:         root,
		started:      time.Now(),
		unknownTypes: map[string]int{},
		warnings:     map[string]int{},
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
	prefixSourceThreadID string,
	prefixBeforeTurn int,
) (string, bool) {
	t.Helper()
	r.events += len(events)
	_, warnings, threadID, logicalRows, buildDuration, applyDuration, err :=
		writer.buildAndApply(events, prefixSourceThreadID, prefixBeforeTurn)
	r.buildDuration += buildDuration
	r.applyDuration += applyDuration
	if err != nil {
		r.fail(t, path, fmt.Errorf("build/apply %s: %w", unit, err))
		return "", false
	}
	r.countWarnings(warnings)
	r.rows += logicalRows
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
	t.Logf("import corpus (%s): stage wall: provider-read=%s branch-convert=%s writer-build=%s store-apply=%s cleanup=%s",
		r.provider,
		r.providerReadDuration.Round(time.Millisecond),
		r.convertDuration.Round(time.Millisecond),
		r.buildDuration.Round(time.Millisecond),
		r.applyDuration.Round(time.Millisecond),
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
