package tracker

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCheckLookupKeys(t *testing.T) {
	many := make([]string, MaxLookupKeys)
	for i := range many {
		many[i] = fmt.Sprintf("WEB-%d", i+1)
	}
	if err := CheckLookupKeys(many); err != nil {
		t.Fatalf("%d keys refused: %v", MaxLookupKeys, err)
	}
	if err := CheckLookupKeys([]string{"WEB-1", "APP-12"}); err != nil {
		t.Fatalf("good keys refused: %v", err)
	}
	bad := map[string][]string{
		"none":         nil,
		"too many":     append(many, "APP-1"),
		"empty":        {"WEB-1", ""},
		"space":        {"WEB 1"},
		"control":      {"WEB-1\x00"},
		"too long":     {strings.Repeat("W", 65)},
		"twice":        {"WEB-1", "WEB-1"},
		"twice folded": {"WEB-1", "web-1"},
	}
	for name, keys := range bad {
		if err := CheckLookupKeys(keys); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: CheckLookupKeys = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestReadInputChecks(t *testing.T) {
	cases := []struct {
		good checker
		bad  map[string]checker
	}{
		{SearchQuery{Text: "login fails", Limit: MaxSearchPage}, map[string]checker{
			"nothing":       SearchQuery{},
			"blank text":    SearchQuery{Text: "   "},
			"long text":     SearchQuery{Text: strings.Repeat("x", 501)},
			"control":       SearchQuery{Text: "a\nb"},
			"negative page": SearchQuery{Text: "x", Limit: -1},
			"page too big":  SearchQuery{Text: "x", Limit: MaxSearchPage + 1},
		}},
		{SearchQuery{Project: "p"}, nil},
		{DownloadInput{Issue: "i", Attachment: "a", To: io.Discard}, map[string]checker{
			"no issue":      DownloadInput{Attachment: "a", To: io.Discard},
			"no attachment": DownloadInput{Issue: "i", To: io.Discard},
			"no writer":     DownloadInput{Issue: "i", Attachment: "a"},
		}},
		{UnlinkInput{Issue: "1", Link: "10001", Other: "2"}, map[string]checker{
			"no issue":  UnlinkInput{Link: "l", Other: "2"},
			"no link":   UnlinkInput{Issue: "1", Other: "2"},
			"no other":  UnlinkInput{Issue: "1", Link: "l"},
			"same side": UnlinkInput{Issue: "1", Link: "l", Other: "1"},
		}},
	}
	for _, c := range cases {
		if err := c.good.Check(); err != nil {
			t.Errorf("%T refused a good input: %v", c.good, err)
		}
		for name, in := range c.bad {
			if err := in.Check(); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%T %s: Check = %v, want ErrInvalidInput", in, name, err)
			}
		}
	}
}
