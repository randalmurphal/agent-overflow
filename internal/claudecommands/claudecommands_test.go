package claudecommands

import (
	"errors"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
)

func key(binary, account string) provider.ProbeCacheKey {
	return provider.ProbeCacheKey{Binary: binary, AccountID: account, WorkDir: "/w"}
}

func commands(names ...string) []provider.SlashCommand {
	out := make([]provider.SlashCommand, 0, len(names))
	for _, name := range names {
		out = append(out, provider.SlashCommand{Name: name})
	}
	return out
}

func names(in []provider.SlashCommand) string {
	out := ""
	for _, cmd := range in {
		out += cmd.Name + ","
	}
	return out
}

func TestStoreAndRead(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")

	if got := cache.CommandsFor(k); got != nil {
		t.Fatalf("pre-probe read = %+v, want nil (unknown, not none)", got)
	}
	if !cache.Store(k, commands("usage", "context"), nil) {
		t.Fatal("first store must report a change")
	}
	if got := names(cache.CommandsFor(k)); got != "usage,context," {
		t.Fatalf("commands = %q", got)
	}
}

// TestStoreReplacesWholesale — there is no merge policy: a command that fell
// off the list is gone, and an empty report clears the entry.
func TestStoreReplacesWholesale(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")
	cache.Store(k, commands("usage", "context", "ship-it"), nil)

	if !cache.Store(k, commands("usage"), nil) {
		t.Fatal("a shorter list is a change")
	}
	if got := names(cache.CommandsFor(k)); got != "usage," {
		t.Fatalf("commands = %q, want the replacement only", got)
	}

	if !cache.Store(k, nil, nil) {
		t.Fatal("clearing to empty is a change")
	}
	if got := cache.CommandsFor(k); got != nil {
		t.Fatalf("commands = %+v, want none after an empty report", got)
	}
}

// TestStoreKeepsPreviousOnDecodeError — an unreadable array is NO information.
func TestStoreKeepsPreviousOnDecodeError(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")
	cache.Store(k, commands("usage"), nil)

	if cache.Store(k, nil, errors.New("boom")) {
		t.Fatal("an unreadable report is not a change")
	}
	if got := names(cache.CommandsFor(k)); got != "usage," {
		t.Fatalf("commands = %q, want the previous list preserved", got)
	}
}

// TestStoreReportsRepeatAsUnchanged so a caller that emits on every probe emits
// once per distinct answer.
func TestStoreReportsRepeatAsUnchanged(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")
	cache.Store(k, commands("usage", "context"), nil)
	if cache.Store(k, commands("usage", "context"), nil) {
		t.Fatal("an identical repeat is not a change")
	}
	if !cache.Store(k, []provider.SlashCommand{{Name: "usage", Description: "now described"}, {Name: "context"}}, nil) {
		t.Fatal("a description change is a change")
	}
}

// TestIdentitySeparation — one identity's list must never be served as
// another's. Project-scoped commands live in the workdir, plugin commands under
// the credentialed home.
func TestIdentitySeparation(t *testing.T) {
	cache := NewCache()
	a := key("claude", "acct-a")
	b := key("claude", "acct-b")
	cache.Store(a, commands("usage"), nil)

	if got := cache.CommandsFor(b); got != nil {
		t.Fatalf("other identity read = %+v, want nil", got)
	}
	cache.Store(b, commands("context"), nil)
	if got := names(cache.CommandsFor(a)); got != "usage," {
		t.Fatalf("identity a = %q", got)
	}
	if got := names(cache.CommandsFor(b)); got != "context," {
		t.Fatalf("identity b = %q", got)
	}
}

func TestEvictsOldestBeyondCap(t *testing.T) {
	cache := NewCache()
	keys := make([]provider.ProbeCacheKey, 0, maxCacheEntries+2)
	for i := 0; i < maxCacheEntries+2; i++ {
		k := key("claude", fmt.Sprintf("acct-%d", i))
		keys = append(keys, k)
		cache.Store(k, commands("usage"), nil)
	}
	if got := cache.CommandsFor(keys[0]); got != nil {
		t.Fatalf("oldest entry survived eviction: %+v", got)
	}
	if got := cache.CommandsFor(keys[len(keys)-1]); got == nil {
		t.Fatal("newest entry was evicted")
	}
}

// TestReadReturnsACopy so a caller mutating what it read cannot corrupt the
// cache for the next reader.
func TestReadReturnsACopy(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")
	cache.Store(k, commands("usage"), nil)

	got := cache.CommandsFor(k)
	got[0].Name = "mutated"
	if again := names(cache.CommandsFor(k)); again != "usage," {
		t.Fatalf("cache mutated through a read: %q", again)
	}
}

// TestStoredSliceIsDetachedFromTheCaller — the same rule on the write side.
func TestStoredSliceIsDetachedFromTheCaller(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")
	in := commands("usage")
	cache.Store(k, in, nil)
	in[0].Name = "mutated"
	if got := names(cache.CommandsFor(k)); got != "usage," {
		t.Fatalf("cache aliased the caller's slice: %q", got)
	}
}

// TestAnswerForSeparatesAnEmptyAnswerFromNoAnswer covers the one thing
// CommandsFor structurally cannot say. Both cases clone back as a nil slice,
// so a caller putting the answer on a wire has to read the reported flag —
// "this binary has no commands" is actionable and "nobody has asked" is not.
func TestAnswerForSeparatesAnEmptyAnswerFromNoAnswer(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")

	if commands, reported := cache.AnswerFor(k); reported || commands != nil {
		t.Fatalf("pre-probe AnswerFor = (%+v, %v), want (nil, false)", commands, reported)
	}

	cache.Store(k, nil, nil)
	if commands, reported := cache.AnswerFor(k); !reported || len(commands) != 0 {
		t.Fatalf("empty-answer AnswerFor = (%+v, %v), want (empty, true)", commands, reported)
	}
}

// TestAnswerForIgnoresAWireError — an unreadable array leaves no entry behind,
// so the identity stays unknown rather than becoming a reported empty list.
func TestAnswerForIgnoresAWireError(t *testing.T) {
	cache := NewCache()
	k := key("claude", "acct-1")

	cache.Store(k, commands("usage"), errors.New("unreadable"))
	if commands, reported := cache.AnswerFor(k); reported || commands != nil {
		t.Fatalf("AnswerFor after a wire error = (%+v, %v), want (nil, false)", commands, reported)
	}
}
