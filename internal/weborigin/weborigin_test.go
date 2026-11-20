package weborigin

import "testing"

func TestOfNormalizesHTTPOrigins(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"http://localhost:8080/?host=webview", "http://localhost:8080", true},
		{"HTTPS://App.Example:443/path#x", "https://app.example:443", true},
		{"file:///C:/x.html", "", false},
		{"about:blank", "", false},
		{"http:///nohost", "", false},
		{"http://%zz", "", false},
	} {
		got, ok := Of(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Of(%q) = %q, %v, want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
