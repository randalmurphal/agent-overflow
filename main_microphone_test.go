package main

import (
	"os"
	"strings"
	"testing"
)

// Dictation opens the microphone from the webview. macOS terminates an app
// that touches it without a usage string, and Capacitor refuses the
// WebView's request unless the manifest declares both permissions it asks
// for.
func TestShellsDeclareMicrophoneUse(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"build/darwin/Info.plist", []string{"<key>NSMicrophoneUsageDescription</key>"}},
		{"build/darwin/Info.dev.plist", []string{"<key>NSMicrophoneUsageDescription</key>"}},
		{"mobile/android/app/src/main/AndroidManifest.xml", []string{
			`<uses-permission android:name="android.permission.RECORD_AUDIO" />`,
			`<uses-permission android:name="android.permission.MODIFY_AUDIO_SETTINGS" />`,
		}},
	} {
		body, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s does not declare %s", tc.path, want)
			}
		}
	}
}
