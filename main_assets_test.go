package main

import (
	"embed"
	"net/http/httputil"
	"testing"
)

//go:embed frontend/dist/index.html
var testAssets embed.FS

// The same call decides both the handler and the CSP the transport ships, so
// the proxy and devProxy must agree: a production binary reporting devProxy
// would relax connect-src on a release build, and a dev proxy not reported
// would have Vite's HMR socket fallback refused by connect-src.
func TestBuildAssetHandlerUsesDevServerWhenAllowed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		devURL    string
		allowDev  bool
		wantProxy bool
	}{
		{"production binary ignores the env var", "http://127.0.0.1:9", false, false},
		{"dev or harness binary proxies the env var", "http://127.0.0.1:9", true, true},
		// Permission to use a dev server is not the same as one running. The
		// harness boots with allowDevAssets=true and usually serves the
		// embedded bundle, and that boot must ship the strict policy.
		{"allowed without the env var serves the bundle", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FRONTEND_DEVSERVER_URL", tc.devURL)

			handler, devProxy, _, err := buildAssetHandler(testAssets, tc.allowDev)
			if err != nil {
				t.Fatalf("buildAssetHandler: %v", err)
			}
			if _, proxied := handler.(*httputil.ReverseProxy); proxied != tc.wantProxy {
				t.Fatalf("handler proxies = %v, want %v", proxied, tc.wantProxy)
			}
			if devProxy != tc.wantProxy {
				t.Fatalf("devProxy = %v, want %v", devProxy, tc.wantProxy)
			}
		})
	}
}
