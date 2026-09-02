package main

import (
	"embed"
	"net/http/httputil"
	"testing"
)

//go:embed frontend/dist/index.html
var testAssets embed.FS

func TestBuildAssetHandlerIgnoresDevServerEnvInProductionBinary(t *testing.T) {
	t.Setenv("FRONTEND_DEVSERVER_URL", "http://127.0.0.1:9")

	handler, devProxy, _, err := buildAssetHandler(testAssets, false)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); ok {
		t.Fatal("production binary should not proxy FRONTEND_DEVSERVER_URL")
	}
	// The same call decides both the handler and the CSP the transport
	// ships. A production binary reporting devProxy would relax
	// connect-src on a release build, which is exactly the drift the
	// single return value exists to prevent.
	if devProxy {
		t.Fatal("production binary reported a dev asset proxy; the transport would ship CSPDevServer")
	}
}

func TestBuildAssetHandlerUsesDevServerWhenAllowed(t *testing.T) {
	t.Setenv("FRONTEND_DEVSERVER_URL", "http://127.0.0.1:9")

	handler, devProxy, _, err := buildAssetHandler(testAssets, true)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); !ok {
		t.Fatal("dev/harness asset handler should proxy FRONTEND_DEVSERVER_URL")
	}
	if !devProxy {
		t.Fatal("dev asset proxy not reported; Vite's HMR socket fallback would be refused by connect-src")
	}
}

func TestBuildAssetHandlerReportsNoDevProxyWithoutTheEnvVar(t *testing.T) {
	// Permission to use a dev server is not the same as one running. The
	// harness boots with allowDevAssets=true and usually serves the
	// embedded bundle, and that boot must ship the strict policy.
	t.Setenv("FRONTEND_DEVSERVER_URL", "")

	handler, devProxy, _, err := buildAssetHandler(testAssets, true)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); ok {
		t.Fatal("no dev server configured, yet the handler proxies")
	}
	if devProxy {
		t.Fatal("no dev server configured, yet devProxy is set")
	}
}
