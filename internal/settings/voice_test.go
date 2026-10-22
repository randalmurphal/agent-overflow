package settings

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDictationAPIOptionsAreOffAndEmptyByDefault(t *testing.T) {
	t.Parallel()
	got := NewService(t.TempDir()).Get()
	if got.DictationAPIEnabled || got.DictationAPIDelay != "" || got.DictationAPILanguages != nil ||
		got.DictationAPIKeywords != nil || got.DictationAPIPrompt != "" {
		t.Fatalf("dictation API options default to %+v; off and empty leaves OpenAI's defaults", got)
	}
}

func TestDictationAPIOptionsRoundTripTidied(t *testing.T) {
	t.Parallel()
	svc := NewService(t.TempDir())
	updated, err := svc.Update(map[string]any{
		"dictationAPIEnabled":   true,
		"dictationAPIDelay":     " low ",
		"dictationAPILanguages": []string{"en", " fr ", "", "en"},
		"dictationAPIKeywords":  []string{"Agent Overflow", "  ", "Svelte", "Svelte"},
		"dictationAPIPrompt":    "  A developer dictating.  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.DictationAPIEnabled || updated.DictationAPIDelay != "low" ||
		!slices.Equal(updated.DictationAPILanguages, []string{"en", "fr"}) ||
		!slices.Equal(updated.DictationAPIKeywords, []string{"Agent Overflow", "Svelte"}) ||
		updated.DictationAPIPrompt != "A developer dictating." {
		t.Fatalf("Update = %+v", updated)
	}
	if again := NewService(filepath.Dir(svc.path)).Get(); !slices.Equal(again.DictationAPIKeywords, updated.DictationAPIKeywords) {
		t.Fatalf("reloaded keywords = %v", again.DictationAPIKeywords)
	}
}

func TestDictationAPIOptionsOpenAIWouldRefuseAreRefused(t *testing.T) {
	t.Parallel()
	tooMany := make([]string, MaxDictationAPIKeywords+1)
	for i := range tooMany {
		tooMany[i] = "kw" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	for name, patch := range map[string]map[string]any{
		"delay":              {"dictationAPIDelay": "instant"},
		"language":           {"dictationAPILanguages": []string{"English"}},
		"keyword with angle": {"dictationAPIKeywords": []string{"<tag>"}},
		"keyword two lines":  {"dictationAPIKeywords": []string{"one\ntwo"}},
		"keyword too long":   {"dictationAPIKeywords": []string{strings.Repeat("k", MaxDictationAPIKeywordRunes+1)}},
		"too many keywords":  {"dictationAPIKeywords": tooMany},
		"prompt too long":    {"dictationAPIPrompt": strings.Repeat("p", MaxDictationAPIPromptBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewService(t.TempDir()).Update(patch); err == nil {
				t.Fatalf("Update(%v) succeeded", patch)
			}
		})
	}
}

// A hand-edited file loads with what OpenAI would refuse dropped, the
// lenient half of the rule above.
func TestAHandEditedDictationAPIFileLoadsWithTheRefusableEntriesDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := `{"dictationAPIEnabled":true,"dictationAPIDelay":"instant","dictationAPILanguages":["en","English","fr"],` +
		`"dictationAPIKeywords":["ok","<bad>","fine"],"dictationAPIPrompt":"` + strings.Repeat("p", MaxDictationAPIPromptBytes+10) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	got := NewService(dir).Get()
	if !got.DictationAPIEnabled || got.DictationAPIDelay != "" ||
		!slices.Equal(got.DictationAPILanguages, []string{"en", "fr"}) ||
		!slices.Equal(got.DictationAPIKeywords, []string{"ok", "fine"}) ||
		len(got.DictationAPIPrompt) != MaxDictationAPIPromptBytes {
		t.Fatalf("loaded %+v", got)
	}
}
