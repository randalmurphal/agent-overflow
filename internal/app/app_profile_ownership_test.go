package app

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

func TestAgentThreadCreationPreservesUserProfiles(t *testing.T) {
	for _, providerName := range []string{"claude", "codex"} {
		t.Run(providerName, func(t *testing.T) {
			f := newThreadToolsFixture(t)
			model := "claude-opus-4-7"
			window := 200000
			if providerName == "codex" {
				model, window = "gpt-5.4", 272000
			}
			profile := store.ChatModelProfile{Provider: providerName, Model: model, ReasoningEffort: "high", ContextWindow: window, RuntimeMode: "full-access", AutoCompactStandardPercent: 71, AutoCompactExtendedPercent: 81, UpdatedAt: 100}
			if err := f.app.store.UpsertChatModelProfile(profile); err != nil {
				t.Fatal(err)
			}
			other := store.ChatModelProfile{Provider: "claude", Model: "claude-sonnet-4-6", ReasoningEffort: "high", ContextWindow: 200000, RuntimeMode: "full-access", UpdatedAt: 200}
			if err := f.app.store.UpsertChatModelProfile(other); err != nil {
				t.Fatal(err)
			}
			adapter := threadToolsApp{app: f.app}
			fast, compact := true, 65
			for _, mode := range []string{"read-only", "approval-required"} {
				spawned, err := adapter.createSpawnedThreadUngrouped(t.Context(), threadtools.SpawnCall{}, CreateThreadOptions{
					ProjectID: f.project.ID, Title: "Agent work", Provider: providerName, Model: model,
					ReasoningEffort: "low", FastMode: &fast, RuntimeMode: mode, ContextWindow: 1000000,
					AutoCompactStandardPercent: &compact, AutoCompactExtendedPercent: &compact,
				})
				if err != nil {
					t.Fatal(err)
				}
				if spawned.RuntimeMode != mode || spawned.ReasoningEffort != "low" || !spawned.FastMode || spawned.ContextWindow != 1000000 || spawned.AutoCompactStandardPercent != compact || spawned.AutoCompactExtendedPercent != compact {
					t.Fatalf("spawn settings lost: %+v", spawned)
				}
				assertSavedProfile(t, f.app, profile)
				latest, err := f.app.store.LatestChatModelProfile()
				if err != nil || latest != other {
					t.Fatalf("agent changed latest selection: %+v, %v", latest, err)
				}
				if err := f.app.store.DeleteThread(spawned.ID); err != nil {
					t.Fatal(err)
				}
				assertSavedProfile(t, f.app, profile)
			}
			next, err := f.app.CreateThread(t.Context(), CreateThreadOptions{ProjectID: f.project.ID, Provider: providerName, Model: model, Title: "User work"})
			if err != nil {
				t.Fatal(err)
			}
			if next.RuntimeMode != profile.RuntimeMode || next.ReasoningEffort != profile.ReasoningEffort || next.FastMode != profile.FastMode || next.ContextWindow != profile.ContextWindow {
				t.Fatalf("new user thread inherited agent settings: %+v", next)
			}
			latest, err := f.app.store.LatestChatModelProfile()
			if err != nil || latest.Provider != providerName || latest.Model != model {
				t.Fatalf("user creation did not remember the selected model: %+v, %v", latest, err)
			}
			// The same explicit settings ARE remembered when chosen by the user.
			_, err = f.app.CreateThread(t.Context(), CreateThreadOptions{
				ProjectID: f.project.ID, Provider: providerName, Model: model,
				ReasoningEffort: "low", FastMode: &fast, RuntimeMode: "read-only",
			})
			if err != nil {
				t.Fatal(err)
			}
			profile.ReasoningEffort, profile.FastMode, profile.RuntimeMode = "low", true, "read-only"
			latest, err = f.app.store.LatestChatModelProfile()
			if err != nil {
				t.Fatal(err)
			}
			profile.UpdatedAt = latest.UpdatedAt
			assertSavedProfile(t, f.app, profile)
			zero, off := 0, false
			_, err = f.app.CreateThread(t.Context(), CreateThreadOptions{
				ProjectID: f.project.ID, Provider: providerName, Model: model, ContextWindow: 1000000, FastMode: &off,
				AutoCompactStandardPercent: &zero, AutoCompactExtendedPercent: &compact,
			})
			if err != nil {
				t.Fatal(err)
			}
			profile.ContextWindow, profile.AutoCompactStandardPercent, profile.AutoCompactExtendedPercent, profile.FastMode = 1000000, 0, compact, false
			latest, err = f.app.store.LatestChatModelProfile()
			if err != nil {
				t.Fatal(err)
			}
			profile.UpdatedAt = latest.UpdatedAt
			assertSavedProfile(t, f.app, profile)
		})
	}
}

func assertSavedProfile(t *testing.T, app *App, want store.ChatModelProfile) {
	t.Helper()
	got, err := app.store.GetChatModelProfile(want.Provider, want.Model)
	if err != nil || got != want {
		t.Fatalf("profile changed: got %+v, want %+v, err=%v", got, want, err)
	}
}

func TestUserProfileControlRemembersOnlySelectedFields(t *testing.T) {
	for _, providerName := range []string{"claude", "codex"} {
		for _, control := range []string{"effort", "fast", "fast-off", "runtime", "context", "model"} {
			t.Run(providerName+"/"+control, func(t *testing.T) {
				f := newThreadToolsFixture(t)
				profile := store.ChatModelProfile{Provider: "claude", Model: "claude-opus-4-7", ReasoningEffort: "high", ContextWindow: 200000, RuntimeMode: "full-access", AutoCompactStandardPercent: 71, AutoCompactExtendedPercent: 81, UpdatedAt: 100}
				if providerName == "codex" {
					profile.Provider, profile.Model, profile.ContextWindow = "codex", "gpt-5.4", 272000
				}
				if err := f.app.store.UpsertChatModelProfile(profile); err != nil {
					t.Fatal(err)
				}
				other := store.ChatModelProfile{Provider: "claude", Model: "claude-sonnet-4-6", ReasoningEffort: "high", ContextWindow: 200000, RuntimeMode: "full-access", UpdatedAt: 200}
				if err := f.app.store.UpsertChatModelProfile(other); err != nil {
					t.Fatal(err)
				}
				thread := f.thread(t, "agent-thread", func(row *store.Thread) {
					row.Provider, row.Model = profile.Provider, profile.Model
					row.ReasoningEffort = "low"
					row.FastMode = true
					row.ContextWindow = 1000000
					row.RuntimeMode = "read-only"
					row.AutoCompactStandardPercent = 60
					row.AutoCompactExtendedPercent = 70
				})
				var err error
				switch control {
				case "effort":
					_, err = f.app.UpdateThreadReasoningEffort(thread.ID, "medium")
					profile.ReasoningEffort = "medium"
				case "fast":
					_, err = f.app.UpdateThreadFastMode(thread.ID, true)
					profile.FastMode = true
				case "fast-off":
					_, err = f.app.UpdateThreadFastMode(thread.ID, false)
					profile.FastMode = false
				case "runtime":
					_, err = f.app.UpdateThreadRuntimeMode(t.Context(), thread.ID, "approval-required")
					profile.RuntimeMode = "approval-required"
				case "context":
					_, err = f.app.UpdateThreadContextSettings(thread.ID, ContextSettingsUpdate{ContextWindow: 1000000, AutoCompactStandardPercent: 63, AutoCompactExtendedPercent: 73})
					profile.ContextWindow, profile.AutoCompactStandardPercent, profile.AutoCompactExtendedPercent = 1000000, 63, 73
				case "model":
					_, err = f.app.UpdateThreadModelSelection(thread.ID, thread.Provider, thread.Model)
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := f.app.store.GetChatModelProfile(profile.Provider, profile.Model)
				if err != nil {
					t.Fatal(err)
				}
				profile.UpdatedAt = got.UpdatedAt
				assertSavedProfile(t, f.app, profile)
				latest, err := f.app.store.LatestChatModelProfile()
				if err != nil || latest != got {
					t.Fatalf("user control did not remember the selected model: %+v, %v", latest, err)
				}
			})
		}
	}
}

func TestUserControlOnUnrememberedAgentModelUsesCleanDefaults(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "unremembered-agent", func(row *store.Thread) {
		row.RuntimeMode, row.ReasoningEffort, row.FastMode = "read-only", "low", true
	})
	want := store.ChatModelProfile{Provider: "claude", Model: "claude-opus-4-7", ReasoningEffort: "medium", ContextWindow: 1000000, RuntimeMode: "full-access"}
	if _, err := f.app.UpdateThreadReasoningEffort(thread.ID, "medium"); err != nil {
		t.Fatal(err)
	}
	got, err := f.app.store.GetChatModelProfile(thread.Provider, thread.Model)
	if err != nil {
		t.Fatal(err)
	}
	want.UpdatedAt = got.UpdatedAt
	assertSavedProfile(t, f.app, want)
}

func TestUserControlReportsProfilePersistenceFailure(t *testing.T) {
	app, path := newTestAppWithStorePath(t)
	thread := createAppTestThread(t, app, "profile-write-failure", "claude", t.TempDir())
	thread.Model = "claude-opus-4-7"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := raw.Exec(`CREATE TRIGGER reject_profile BEFORE INSERT ON chat_model_profiles BEGIN SELECT RAISE(ABORT,'injected profile failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateThreadRuntimeMode(t.Context(), thread.ID, "read-only"); err == nil || !strings.Contains(err.Error(), "injected profile failure") {
		t.Fatalf("profile save failure = %v", err)
	}
}

func TestConcurrentUserControlsPreserveIndependentPreferences(t *testing.T) {
	f := newThreadToolsFixture(t)
	threads := make([]store.Thread, 4)
	for i := range threads {
		threads[i] = f.thread(t, fmt.Sprintf("concurrent-control-%d", i))
	}
	base := f.app.sanitizeChatModelProfile(f.app.fallbackChatModelProfile(threads[0].Provider, threads[0].Model))
	for range 10 {
		if err := f.app.store.UpsertChatModelProfile(base); err != nil {
			t.Fatal(err)
		}
		start, results := make(chan struct{}), make(chan error, 4)
		for i := range threads {
			go func() {
				<-start
				var err error
				switch i {
				case 0:
					_, err = f.app.UpdateThreadReasoningEffort(threads[i].ID, "low")
				case 1:
					_, err = f.app.UpdateThreadFastMode(threads[i].ID, true)
				case 2:
					_, err = f.app.UpdateThreadRuntimeMode(t.Context(), threads[i].ID, "read-only")
				case 3:
					_, err = f.app.UpdateThreadContextSettings(threads[i].ID, ContextSettingsUpdate{ContextWindow: 1000000, AutoCompactStandardPercent: 63, AutoCompactExtendedPercent: 73})
				}
				results <- err
			}()
		}
		close(start)
		for range threads {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		got, err := f.app.store.GetChatModelProfile(base.Provider, base.Model)
		if err != nil {
			t.Fatal(err)
		}
		want := base
		want.ReasoningEffort, want.FastMode, want.RuntimeMode = "low", true, "read-only"
		want.ContextWindow, want.AutoCompactStandardPercent, want.AutoCompactExtendedPercent, want.UpdatedAt = 1000000, 63, 73, got.UpdatedAt
		assertSavedProfile(t, f.app, want)
	}
}

func TestUserCreationPublishesRememberedDefaults(t *testing.T) {
	app := newTestAppWithStore(t)
	published := false
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		event, ok := data.(triage.ThreadUpdateEvent)
		if !ok || event.Action != triage.ThreadActionListed || event.Thread == nil || event.Thread.Title != "User selection" {
			return
		}
		published = true
		profile, err := app.store.LatestChatModelProfile()
		if err != nil || profile.Model != "claude-opus-4-7" || profile.RuntimeMode != "read-only" || profile.ReasoningEffort != "low" {
			t.Errorf("announced a thread before remembering its selection: %+v, %v", profile, err)
		}
		if reason, err := app.workAdmission.quiesce(func() (string, error) { return "", nil }); err != nil || reason == "" {
			t.Errorf("creation was not protected from host handoff: %q, %v", reason, err)
		}
	}
	_, err := app.CreateThread(t.Context(), CreateThreadOptions{ProjectID: defaultTestProjectID, Title: "User selection", Provider: "claude", Model: "claude-opus-4-7", RuntimeMode: "read-only", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("created thread was not announced")
	}
}

func TestAgentCreationDoesNotIntroduceAnUnrememberedProfile(t *testing.T) {
	for _, providerName := range []string{"claude", "codex"} {
		t.Run(providerName, func(t *testing.T) {
			f := newThreadToolsFixture(t)
			model := "claude-opus-4-7"
			if providerName == "codex" {
				model = "gpt-5.4"
			}
			_, err := (threadToolsApp{app: f.app}).createSpawnedThreadUngrouped(t.Context(), threadtools.SpawnCall{}, CreateThreadOptions{
				ProjectID: f.project.ID, Title: "Unremembered agent", Provider: providerName, Model: model, RuntimeMode: "read-only", ReasoningEffort: "low",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.store.GetChatModelProfile(providerName, model); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("agent introduced a model preference: %v", err)
			}
			if _, err := f.app.store.LatestChatModelProfile(); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("agent introduced a last-selected model: %v", err)
			}
		})
	}
}

func TestAgentOverridesDoNotSurviveAsDefaultsAfterRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	path := storetest.ClonePath(t)
	initial, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := initial.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	app.store = initial
	ensureDefaultTestProject(t, app)
	profile := store.ChatModelProfile{Provider: "claude", Model: "claude-opus-4-7", ReasoningEffort: "high", ContextWindow: 200000, RuntimeMode: "full-access", AutoCompactStandardPercent: 71, AutoCompactExtendedPercent: 81, UpdatedAt: 100}
	if err := app.store.UpsertChatModelProfile(profile); err != nil {
		t.Fatal(err)
	}
	_, err = (threadToolsApp{app: app}).createSpawnedThreadUngrouped(t.Context(), threadtools.SpawnCall{}, CreateThreadOptions{
		ProjectID: defaultTestProjectID, Title: "Temporary reader", Provider: profile.Provider, Model: profile.Model, RuntimeMode: "read-only", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	restarted := newTestAppWithStore(t)
	restarted.store = reopened
	assertSavedProfile(t, restarted, profile)
	defaults, err := restarted.GetThreadDefaults(CreateThreadOptions{ProjectID: defaultTestProjectID})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Provider != profile.Provider || defaults.Model != profile.Model || defaults.RuntimeMode != "full-access" || defaults.ReasoningEffort != "high" || defaults.FastMode || defaults.ContextWindow != 200000 {
		t.Fatalf("reopened app adopted agent settings: %+v", defaults)
	}
}
