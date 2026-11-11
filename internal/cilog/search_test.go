package cilog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSearchCountsMatchesAsTheViewShowsLines(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	lines := []string{
		"Needle at the start",
		"no match here",
		"\x1b[31mnee\x1b[0mdle split by color, NEEDLE again",
		"needleneedle",
		"\x1b]0;needle in a title\x07plain",
		"ΣNEEDLÉ needlé",
	}
	fetchText(t, s, key, strings.Join(lines, "\n")+"\n", `"a"`)
	got, err := s.Search(t.Context(), key, "  needle ", 100, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	want := []SearchHit{{Line: 0, Count: 1}, {Line: 2, Count: 2}, {Line: 3, Count: 2}}
	if !slices.Equal(got.Hits, want) || got.Matches != 5 || got.Capped || got.TotalLines != len(lines) {
		t.Fatalf("search = %+v, want hits %v", got, want)
	}
	got, err = s.Search(t.Context(), key, "NEEDLÉ", 100, 1<<16)
	if err != nil || !slices.Equal(got.Hits, []SearchHit{{Line: 5, Count: 2}}) {
		t.Fatalf("non-ASCII search = %+v, %v", got, err)
	}
}

func TestSearchStopsAtItsLimitAndTheLineBound(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	lines := []string{"ab ab ab", "ab", strings.Repeat("x", 100) + "ab"}
	fetchText(t, s, key, strings.Join(lines, "\n")+"\n", `"a"`)
	got, err := s.Search(t.Context(), key, "ab", 2, 1<<16)
	if err != nil || !slices.Equal(got.Hits, []SearchHit{{Line: 0, Count: 2}}) || got.Matches != 2 || !got.Capped {
		t.Fatalf("capped search = %+v, %v", got, err)
	}
	// Text past the shown part of a line is not searched.
	got, err = s.Search(t.Context(), key, "ab", 10, 50)
	if err != nil || !slices.Equal(got.Hits, []SearchHit{{Line: 0, Count: 3}, {Line: 1, Count: 1}}) || got.Capped {
		t.Fatalf("bounded search = %+v, %v", got, err)
	}
}

func TestSearchEndsWithItsContext(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	fetchText(t, s, key, strings.Join(numberedLines(3000), "\n")+"\n", `"a"`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Search(ctx, key, "line", 10, 1<<16); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search = %v, want context.Canceled", err)
	}
	if _, err := s.Search(t.Context(), githubKey("404"), "line", 10, 1<<16); !errors.Is(err, ErrNoLog) {
		t.Fatalf("search of an unknown log = %v, want ErrNoLog", err)
	}
	if _, err := s.Search(t.Context(), key, "   ", 10, 1<<16); err == nil {
		t.Fatal("a blank query searched")
	}
}

func TestFoldForFindKeepsUTF16Length(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"ABC":   "abc",
		"ÀÉÎ":   "àéî",
		"İx":    "İx",
		"ΣΑΣ":   "σασ",
		"\xffA": "\xffa",
	} {
		if got := FoldForFind(in); got != want {
			t.Errorf("FoldForFind(%q) = %q, want %q", in, got, want)
		}
	}
}
