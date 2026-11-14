package app

import (
	"testing"

	"agent-overflow/internal/claudecatalog"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

// storeClaudeReport stores rows as the Claude CLI's model report for app's
// probe identity, the way the account probe does. claudecatalog is
// process-wide, so a test that calls it must not call t.Parallel and must
// reset the catalog first (resetClaudeReports or seedClaudeFastModels).
func storeClaudeReport(t *testing.T, app *App, rows ...claude.WireModel) {
	t.Helper()
	capture := claudecatalog.ModelCapture{}
	capture.Capture(rows, nil)
	capture.Store(app.claudeProbeModelKey())
}

// resetClaudeReports clears every stored Claude model report for this test
// and after it. Serial tests only.
func resetClaudeReports(t *testing.T) {
	t.Helper()
	claudecatalog.Reset()
	t.Cleanup(claudecatalog.Reset)
}

// seedClaudeFastModels makes a report marking each model fast the only
// Claude report. Shipped Claude models state no fast mode, so a test that
// needs a fast-capable Claude model seeds one. Each row reports a shipped
// model's own efforts, so only fast mode differs. Serial tests only.
func seedClaudeFastModels(t *testing.T, app *App, models ...string) {
	t.Helper()
	resetClaudeReports(t)
	rows := make([]claude.WireModel, 0, len(models))
	for _, model := range models {
		row := claude.WireModel{Value: model, SupportsFastMode: true}
		if shipped, found := provider.FindModel(string(provider.Claude), model); found {
			for _, effort := range shipped.ReasoningEfforts {
				row.SupportedEffortLevels = append(row.SupportedEffortLevels, effort.Slug)
			}
			row.SupportsEffort = len(row.SupportedEffortLevels) > 0
		}
		rows = append(rows, row)
	}
	storeClaudeReport(t, app, rows...)
}

// The models the fast-mode tests reason about.
const (
	// reportedFastModel is reported fast and is not in the shipped list.
	reportedFastModel = "claude-newthing-1"
	// reportedSlowModel is reported without fast mode.
	reportedSlowModel = "claude-opus-4-7"
	// omittedModel is shipped and left out of the report, like the CLI's
	// shortlist leaves out older models.
	omittedModel = "claude-opus-4-8"
)

func storeFastModeReport(t *testing.T, app *App) {
	t.Helper()
	if candidate, found := provider.FindModel(string(provider.Claude), reportedFastModel); found {
		t.Fatalf("fixture model is in the shipped list: %+v", candidate)
	}
	storeClaudeReport(t, app,
		claude.WireModel{Value: reportedFastModel, SupportsFastMode: true},
		claude.WireModel{Value: reportedSlowModel},
	)
}

type fastModeFixture struct {
	t       *testing.T
	app     *App
	project store.Project
}

func newFastModeFixture(t *testing.T) fastModeFixture {
	t.Helper()
	resetClaudeReports(t)
	app := newTestAppWithStore(t)
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	return fastModeFixture{t: t, app: app, project: project}
}

// createFast asks for a new thread on model with fast mode on.
func (f fastModeFixture) createFast(model string) (store.Thread, error) {
	fast := true
	return f.app.CreateThread(f.t.Context(), CreateThreadOptions{
		ProjectID: f.project.ID, Provider: string(provider.Claude), Model: model, FastMode: &fast,
	})
}

// savedFastThread stores a thread row on model that already carries fast
// mode, as one saved under an earlier catalog would.
func (f fastModeFixture) savedFastThread(model string) store.Thread {
	f.t.Helper()
	thread, err := f.app.CreateThread(f.t.Context(), CreateThreadOptions{
		ProjectID: f.project.ID, Provider: string(provider.Claude), Model: model,
	})
	if err != nil {
		f.t.Fatalf("CreateThread(%s): %v", model, err)
	}
	thread.FastMode = true
	if err := f.app.store.UpdateThread(thread); err != nil {
		f.t.Fatalf("UpdateThread: %v", err)
	}
	return thread
}

// keptBySanitize reports whether a saved profile on model keeps fast mode.
func (f fastModeFixture) keptBySanitize(model string) bool {
	return f.app.sanitizeChatModelProfile(store.ChatModelProfile{
		Provider: string(provider.Claude), Model: model, FastMode: true,
	}).FastMode
}

// keptByFocus reports whether focusing the thread keeps its fast mode.
func (f fastModeFixture) keptByFocus(threadID string) bool {
	f.t.Helper()
	focused, err := f.app.loadThreadForFocus(threadID)
	if err != nil {
		f.t.Fatalf("loadThreadForFocus: %v", err)
	}
	return focused.FastMode
}

// launchesFast builds the Claude launch config the way startSessionNow does
// and reports whether it carries fast mode.
func (f fastModeFixture) launchesFast(threadID string) bool {
	f.t.Helper()
	row, err := f.app.store.GetThread(threadID)
	if err != nil {
		f.t.Fatalf("GetThread: %v", err)
	}
	opts, err := f.app.buildSessionOptions(f.app.sanitizeThreadModelSettings(row))
	if err != nil {
		f.t.Fatalf("buildSessionOptions: %v", err)
	}
	return claude.ConfigFromOptions(opts).FastMode
}

// Before the CLI has reported, AO cannot know which Claude models run fast:
// a new fast selection is refused, and a saved one stands through sanitize,
// focus and launch.
func TestClaudeFastModeWithoutAReport(t *testing.T) {
	// Serial: seeds the process-wide Claude model report.
	f := newFastModeFixture(t)
	claudeName := string(provider.Claude)

	if f.app.supportsFastModeForModel(claudeName, reportedSlowModel) {
		t.Fatal("a model nothing reported fast is offered fast mode")
	}
	if _, err := f.createFast(reportedSlowModel); err == nil {
		t.Fatal("creating a fast thread with no report succeeded")
	}
	thread := f.savedFastThread(reportedSlowModel)
	if _, err := f.app.UpdateThreadFastMode(thread.ID, true); err == nil {
		t.Fatal("turning fast mode on with no report succeeded")
	}

	if !f.keptBySanitize(reportedSlowModel) {
		t.Error("sanitize cleared a saved fast flag with no report")
	}
	if !f.keptByFocus(thread.ID) {
		t.Error("focus cleared a saved fast flag with no report")
	}
	if !f.launchesFast(thread.ID) {
		t.Error("launch dropped a saved fast flag with no report")
	}
}

// Once the CLI has reported, fast mode is offered, kept and launched exactly
// on the models the report marks fast.
func TestClaudeFastModeFollowsTheReport(t *testing.T) {
	// Serial: seeds the process-wide Claude model report.
	f := newFastModeFixture(t)
	storeFastModeReport(t, f.app)
	claudeName := string(provider.Claude)

	t.Run("reported fast", func(t *testing.T) {
		if !f.app.supportsFastModeForModel(claudeName, reportedFastModel) {
			t.Fatal("a model the CLI reports fast is refused fast mode")
		}
		thread, err := f.createFast(reportedFastModel)
		if err != nil {
			t.Fatalf("CreateThread with fast mode: %v", err)
		}
		if !thread.FastMode {
			t.Fatal("created thread lost its fast mode")
		}
		if !f.keptByFocus(thread.ID) {
			t.Error("focus cleared fast mode the report grants")
		}
		if !f.launchesFast(thread.ID) {
			t.Error("launch dropped fast mode the report grants")
		}
	})

	for _, model := range []string{reportedSlowModel, omittedModel} {
		t.Run("not reported fast/"+model, func(t *testing.T) {
			if f.app.supportsFastModeForModel(claudeName, model) {
				t.Fatal("offered fast mode the report does not grant")
			}
			if _, err := f.createFast(model); err == nil {
				t.Fatal("creating a fast thread succeeded")
			}
			if f.keptBySanitize(model) {
				t.Error("sanitize kept a saved fast flag the report does not grant")
			}
			thread := f.savedFastThread(model)
			if f.launchesFast(thread.ID) {
				t.Error("launch carries fast mode the report does not grant")
			}
		})
	}
}

// An answer that omits a model carries no information about it, so fast
// mode an earlier answer for the same identity reported stays.
func TestClaudeFastModeSurvivesAnAnswerThatOmitsTheModel(t *testing.T) {
	// Serial: seeds the process-wide Claude model report.
	f := newFastModeFixture(t)
	const shippedFast = "claude-opus-5"
	storeClaudeReport(t, f.app,
		claude.WireModel{Value: reportedFastModel, SupportsFastMode: true},
		claude.WireModel{Value: shippedFast, SupportsFastMode: true},
	)
	threads := map[string]store.Thread{}
	for _, model := range []string{reportedFastModel, shippedFast} {
		thread, err := f.createFast(model)
		if err != nil {
			t.Fatalf("CreateThread(%s) with fast mode: %v", model, err)
		}
		threads[model] = thread
	}

	storeClaudeReport(t, f.app, claude.WireModel{Value: reportedSlowModel})

	for model, thread := range threads {
		if !f.app.supportsFastModeForModel(string(provider.Claude), model) {
			t.Errorf("%s: an answer omitting it withdrew fast mode", model)
		}
		if !f.keptBySanitize(model) {
			t.Errorf("%s: sanitize cleared fast mode after an omitting answer", model)
		}
		if !f.launchesFast(thread.ID) {
			t.Errorf("%s: launch dropped fast mode after an omitting answer", model)
		}
	}
}
