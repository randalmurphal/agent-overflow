package app

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/testutil/reporoot"
)

// Values a settings read withholds from a caller without access:admin
// (settingsForCaller). Each one stands for a credential the field can hold.
const (
	adminReadHookSecret = "--token=dns-hook-secret"
	adminReadOTLPSecret = "collector.test:4317?api-key=otlp-secret"
)

// adminReadApp is an App with identity, holding a DNS hook and an OTLP
// endpoint.
func adminReadApp(t *testing.T) *App {
	t.Helper()
	app := accessApp(t)
	setAdminReadHook(t, app)
	if _, err := app.settings.Update(map[string]any{"observabilityOtlpEndpoint": adminReadOTLPSecret}); err != nil {
		t.Fatalf("Update observabilityOtlpEndpoint: %v", err)
	}
	return app
}

// setAdminReadHook saves a DNS hook. A build without remote access refuses
// the remote network fields, so there the network block has nothing to
// withhold and the OTLP endpoint is the value under test.
func setAdminReadHook(t *testing.T, app *App) {
	t.Helper()
	if !buildvariant.RemoteAccess {
		return
	}
	if _, err := app.settings.SetNetwork(settings.NetworkSettings{
		CanonicalDomain: "backend.example.test",
		ACMEDNSHook:     []string{"/usr/local/bin/dns-hook", adminReadHookSecret},
	}); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
}

// settingsReads calls every bound method that returns a Settings value.
// TestEverySettingsReturningMethodProjectsForItsCaller keeps this list
// complete. The writes each move a value so they succeed on repeat calls.
func settingsReads(t *testing.T, app *App, ctx context.Context) map[string]settings.Settings {
	t.Helper()
	got := map[string]settings.Settings{}
	read := func(name string, call func() (settings.Settings, error)) {
		value, err := call()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[name] = value
	}
	read("GetSettings", func() (settings.Settings, error) { return app.GetSettings(ctx) })
	read("UpdateSettings", func() (settings.Settings, error) {
		return app.UpdateSettings(ctx, map[string]any{"fontSize": 15})
	})
	read("SetProviderCustomEnvVar", func() (settings.Settings, error) {
		return app.SetProviderCustomEnvVar(ctx, "claude", "ADMIN_READ_VAR", "v", false)
	})
	read("DeleteProviderCustomEnvVar", func() (settings.Settings, error) {
		return app.DeleteProviderCustomEnvVar(ctx, "claude", "ADMIN_READ_VAR")
	})
	return got
}

func TestSettingsReadsWithholdAdminValuesFromAViewOnlyDevice(t *testing.T) {
	t.Parallel()
	app := adminReadApp(t)
	view := mintForAccess(t, app, string(identity.PairingAccessViewOnly), "thumb-view")

	for method, got := range settingsReads(t, app, callFrom(view.SessionID, false)) {
		if !zeroNetwork(got.Network) {
			t.Errorf("%s gave a view-only device the network block: %+v", method, got.Network)
		}
		if got.ObservabilityOtlpEndpoint != "" {
			t.Errorf("%s gave a view-only device the OTLP endpoint %q", method, got.ObservabilityOtlpEndpoint)
		}
		// The rest of the host tier still renders what this backend enables.
		if !got.ClaudeEnabled {
			t.Errorf("%s withheld claudeEnabled from a view-only device", method)
		}
	}
}

func TestSettingsReadsKeepAdminValuesForAdminAndLocalCallers(t *testing.T) {
	t.Parallel()
	app := adminReadApp(t)
	admin := mintForAccess(t, app, string(identity.PairingAccessFull), "thumb-admin")

	for caller, ctx := range map[string]context.Context{
		"admin device":         callFrom(admin.SessionID, false),
		"local page channel":   callFrom(app.identityState().local.SessionID, true),
		"sessionless in-place": context.Background(),
	} {
		for method, got := range settingsReads(t, app, ctx) {
			if buildvariant.RemoteAccess && !slices.Contains(got.Network.ACMEDNSHook, adminReadHookSecret) {
				t.Errorf("%s via %s: network = %+v, want the DNS hook", caller, method, got.Network)
			}
			if got.ObservabilityOtlpEndpoint != adminReadOTLPSecret {
				t.Errorf("%s via %s: OTLP endpoint = %q, want %q", caller, method, got.ObservabilityOtlpEndpoint, adminReadOTLPSecret)
			}
		}
	}
}

// No event carries a settings value, so a connection without access:admin
// cannot receive one by subscription either; it re-reads through
// GetSettings and gets its own projection.
func TestSettingsWritesEmitNoSettingsValues(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	var frames []string
	app.emitEventFn = func(name string, data any) {
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Errorf("marshal %s payload: %v", name, err)
			return
		}
		frames = append(frames, name+" "+string(encoded))
	}
	const envSecret = "env-secret-value"
	if _, err := app.SetProviderCustomEnvVar(context.Background(), "claude", "MY_VAR", envSecret, true); err != nil {
		t.Fatalf("SetProviderCustomEnvVar: %v", err)
	}
	setAdminReadHook(t, app)
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"observabilityOtlpEndpoint": adminReadOTLPSecret}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("the writes emitted no frame")
	}
	for _, frame := range frames {
		for _, secret := range []string{envSecret, adminReadHookSecret, adminReadOTLPSecret} {
			if strings.Contains(frame, secret) {
				t.Errorf("frame carried a settings value: %s", frame)
			}
		}
	}
}

// Every exported App method returning a Settings value projects it through
// settingsForCaller, and settingsReads exercises each one.
func TestEverySettingsReturningMethodProjectsForItsCaller(t *testing.T) {
	t.Parallel()
	const packageDir = "internal/app"
	fset := token.NewFileSet()
	var methods []string
	for _, path := range reporoot.Files(t, packageDir, "*.go") {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := reporoot.ParseGoFile(t, fset, path, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !fn.Name.IsExported() || !returnsSettings(fn) {
				continue
			}
			methods = append(methods, fn.Name.Name)
			if !callsSettingsForCaller(fn.Body) {
				t.Errorf("%s returns settings.Settings without settingsForCaller", fn.Name.Name)
			}
		}
	}
	slices.Sort(methods)
	want := []string{"DeleteProviderCustomEnvVar", "GetSettings", "SetProviderCustomEnvVar", "UpdateSettings"}
	if !slices.Equal(methods, want) {
		t.Errorf("Settings-returning methods = %v; add the new one to settingsReads and to %v", methods, want)
	}
}

func zeroNetwork(n settings.NetworkSettings) bool {
	return reflect.DeepEqual(n, settings.NetworkSettings{})
}

func returnsSettings(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "settings" && sel.Sel.Name == "Settings" {
			return true
		}
	}
	return false
}

func callsSettingsForCaller(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "settingsForCaller" {
			found = true
		}
		return !found
	})
	return found
}
