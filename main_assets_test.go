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

	originalMode := nativeMode
	t.Cleanup(func() { nativeMode = originalMode })
	nativeMode = "prod"

	handler, err := buildAssetHandler(testAssets)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); ok {
		t.Fatal("production binary should not proxy FRONTEND_DEVSERVER_URL")
	}
}

func TestBuildAssetHandlerUsesDevServerForDevMode(t *testing.T) {
	t.Setenv("FRONTEND_DEVSERVER_URL", "http://127.0.0.1:9")

	originalMode := nativeMode
	t.Cleanup(func() { nativeMode = originalMode })
	nativeMode = "dev"

	handler, err := buildAssetHandler(testAssets)
	if err != nil {
		t.Fatalf("buildAssetHandler: %v", err)
	}
	if _, ok := handler.(*httputil.ReverseProxy); !ok {
		t.Fatal("dev binary should proxy FRONTEND_DEVSERVER_URL")
	}
}
