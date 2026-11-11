package cilog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	// Each code point folds: É matches é.
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

// The frontend's findMatch.test.ts reads the same vectors.
func TestFindRuleMatchesTheSharedVectors(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../frontend/src/test/fixtures/findVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Count []struct {
			Text  string `json:"text"`
			Query string `json:"query"`
			Count int    `json:"count"`
		} `json:"count"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Count) == 0 {
		t.Fatal("no vectors")
	}
	s, _, _ := newTestStore(t, CacheBytes)
	for i, v := range vectors.Count {
		key := githubKey(fmt.Sprint(100 + i))
		fetchText(t, s, key, v.Text+"\n", `"a"`)
		got, err := s.Search(t.Context(), key, v.Query, 100, 1<<16)
		if strings.TrimSpace(v.Query) == "" {
			if err == nil {
				t.Errorf("blank query %q searched", v.Query)
			}
			continue
		}
		if err != nil || got.Matches != v.Count {
			t.Errorf("Search(%q in %q) = %d, %v; want %d", v.Query, v.Text, got.Matches, err, v.Count)
		}
	}
}

func TestSearchMatchesInvalidUTF8AsTheViewReceivesIt(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("utf8")
	fetchText(t, s, key, "a\xff\xfeB\n", `"a"`)
	got, err := s.Search(t.Context(), key, "a\uFFFD\uFFFDb", 10, 1<<16)
	if err != nil || got.Matches != 1 {
		t.Fatalf("Search = %+v, %v; want one match", got, err)
	}
}
