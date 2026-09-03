package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/settings"
)

// settingsBroadcasts captures the `settings:updated` frames one mutation
// produced, in emit order.
type settingsBroadcasts struct {
	t      *testing.T
	events []SettingsUpdatedEvent
}

func captureSettingsBroadcasts(t *testing.T, app *App) *settingsBroadcasts {
	t.Helper()
	recorder := &settingsBroadcasts{t: t}
	app.emitEventFn = func(name string, data any) {
		if name != "settings:updated" {
			return
		}
		evt, ok := data.(SettingsUpdatedEvent)
		if !ok {
			t.Errorf("settings:updated payload type = %T, want app.SettingsUpdatedEvent", data)
			return
		}
		recorder.events = append(recorder.events, evt)
	}
	return recorder
}

func TestUpdateSettingsBroadcastsOneFramePerTier(t *testing.T) {
	app := newTestApp(t)
	broadcasts := captureSettingsBroadcasts(t, app)

	if _, err := app.UpdateSettings(context.Background(), map[string]any{"fontSize": 17, "confirmDelete": false}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if len(broadcasts.events) != 2 {
		t.Fatalf("emitted %d frames, want one per tier touched: %+v", len(broadcasts.events), broadcasts.events)
	}
	if got := broadcasts.events[0]; got.Tier != "user" || !slices.Equal(got.Keys, []string{"confirmDelete"}) {
		t.Errorf("frame 0 = %+v", got)
	}
	if got := broadcasts.events[1]; got.Tier != "device" || !slices.Equal(got.Keys, []string{"fontSize"}) {
		t.Errorf("frame 1 = %+v", got)
	}
}

func TestUpdateSettingsWithNoChangeBroadcastsNothing(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	broadcasts := captureSettingsBroadcasts(t, app)
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"fontSize": 17}); err != nil {
		t.Fatalf("repeat UpdateSettings: %v", err)
	}
	if len(broadcasts.events) != 0 {
		t.Fatalf("repeat save broadcast %+v", broadcasts.events)
	}
}

// Every persisted settings surface broadcasts, not just the generic patch
// binding. SetEditorSettings and the custom-environment CRUD write through
// their own service methods, which is exactly the shape a per-call-site emit
// would have missed.
func TestEverySettingsBindingBroadcasts(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		write func(*App) error
		tier  string
		keys  []string
	}{
		{
			name: "SetEditorSettings",
			write: func(app *App) error {
				_, err := app.SetEditorSettings(settings.EditorSettings{Preference: "code"})
				return err
			},
			tier: "user",
			keys: []string{"editor"},
		},
		{
			name: "SetProviderCustomEnvVar",
			write: func(app *App) error {
				_, err := app.SetProviderCustomEnvVar("claude", "MY_VAR", "value", false)
				return err
			},
			tier: "host",
			keys: []string{"claudeCustomEnv"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			broadcasts := captureSettingsBroadcasts(t, app)
			if err := testCase.write(app); err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}
			if len(broadcasts.events) != 1 {
				t.Fatalf("emitted %d frames, want 1: %+v", len(broadcasts.events), broadcasts.events)
			}
			got := broadcasts.events[0]
			if got.Tier != testCase.tier || !slices.Equal(got.Keys, testCase.keys) {
				t.Fatalf("frame = %+v, want tier %q keys %v", got, testCase.tier, testCase.keys)
			}
		})
	}
}

// The frame names keys. Values never ride it, because GetSettings redacts
// endpoint tokens and sensitive environment values and this channel must not
// become the way around that.
func TestSettingsBroadcastCarriesNoValues(t *testing.T) {
	app := newTestApp(t)
	broadcasts := captureSettingsBroadcasts(t, app)
	const secret = "s3cret-value"
	if _, err := app.SetProviderCustomEnvVar("claude", "MY_VAR", secret, true); err != nil {
		t.Fatalf("SetProviderCustomEnvVar: %v", err)
	}
	for _, evt := range broadcasts.events {
		if slices.Contains(evt.Keys, secret) {
			t.Fatalf("frame carried the value: %+v", evt)
		}
	}
}

// An App holding a settings service whose writes nothing announces is the one
// way this feature regresses silently, so production code installs the service
// through the single helper that also arms the broadcast. Test fixtures are
// exempt on purpose: a fixture that never emits is a fixture, not a defect.
func TestSettingsServiceIsInstalledThroughOneHelper(t *testing.T) {
	// This suite runs from the repository root (see TestMain), so the package
	// is named explicitly rather than scanned as ".".
	const packageDir = "internal/app"
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var assigners []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(packageDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				assign, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, target := range assign.Lhs {
					sel, ok := target.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "settings" {
						assigners = append(assigners, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(assigners)
	assigners = slices.Compact(assigners)
	if !slices.Equal(assigners, []string{"setSettingsService"}) {
		t.Fatalf("App.settings assigners = %v, want exactly [setSettingsService]", assigners)
	}
}
