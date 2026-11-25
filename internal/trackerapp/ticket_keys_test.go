package trackerapp

import (
	"slices"
	"testing"
)

func TestTicketKeysNamesEachKeyOnceBetweenBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"WEB-1 Retry cursor", []string{"WEB-1"}},
		{"web-1-retry-cursor", []string{"WEB-1"}},
		{"feature/WEB-12_fix", []string{"WEB-12"}},
		{"[WEB-1] and WEB-2, again web-1", []string{"WEB-1", "WEB-2"}},
		{"APP_2-40: (ENG-7)", []string{"APP_2-40", "ENG-7"}},
		{"WEB-12 is not WEB-1", []string{"WEB-12", "WEB-1"}},
		{"see.WEB-1", []string{"WEB-1"}},
		{"WEB-1x", nil},
		{"WEB-01", nil},
		{"WEB-", nil},
		{"W-1", nil},
		{"1WEB-1", nil},
		{"utf-8 and iso-8859-1", []string{"UTF-8", "ISO-8859"}},
		{"", nil},
	} {
		if got := TicketKeys(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("TicketKeys(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
