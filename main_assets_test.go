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

	handler, err := buildAssetHandler(testAssets, false)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); ok {
		t.Fatal("production binary should not proxy FRONTEND_DEVSERVER_URL")
	}
}

func TestBuildAssetHandlerUsesDevServerWhenAllowed(t *testing.T) {
	t.Setenv("FRONTEND_DEVSERVER_URL", "http://127.0.0.1:9")

	handler, err := buildAssetHandler(testAssets, true)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); !ok {
		t.Fatal("dev/harness asset handler should proxy FRONTEND_DEVSERVER_URL")
	}
}
