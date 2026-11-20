package git

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/stringsx"
)

// The forge facts cache's bounds and lifetimes: an entry per key, host
// and login; a found fact answers for 10 minutes and an absent one for 2,
// so a just-created issue shows soon; an unavailable one is never held.
const (
	forgeFactCacheEntries = 4096
	forgeFactCacheBytes   = 4 << 20
	forgeFactFoundTTL     = 10 * time.Minute
	forgeFactAbsentTTL    = 2 * time.Minute
	// forgeFactsTimeout bounds one call's forge reads, within the
	// method's default budget beside its git reads.
	forgeFactsTimeout = 20 * time.Second
	// maxFactTitleBytes bounds a title a fact carries; the forges cap
	// titles at 255 characters.
	maxFactTitleBytes = 1024
)

// newForgeFactCache is a forge adapter's facts cache: keyed by forge,
// host, login fingerprint and key, at most forgeFactCacheEntries entries
// and forgeFactCacheBytes estimated bytes, least recently used evicted
// first (factCache). An entry may hold a label list as well as the
// fields a single fact's 1/64 of the bytes allows.
func newForgeFactCache() *factCache[ForgeFact] {
	cache := newFactCache(forgeFactCacheEntries, forgeFactCacheBytes, factBytes)
	cache.maxEntryBytes = forgeFactCacheBytes/64 + maxLabelListBytes
	return cache
}

// factSender reads keys from the forge: one fact per key in keys' order,
// and the credential fingerprint the answer was read with (empty when no
// request answered).
type factSender func(ctx context.Context, client *forgeapi.Client, pr PRReference, keys []forgeFactKey) ([]ForgeFact, string)

// mixedCredential is a factSender credential no login has: its reads
// answered under two, so nothing they read is held.
const mixedCredential = "\x00mixed"

// joinCredential is the credential reads answered under, held so far,
// after one more answered under answered (empty when no request
// answered): mixedCredential once two differ.
func joinCredential(held, answered string) string {
	switch {
	case answered == "" || answered == held:
		return held
	case held == "":
		return answered
	}
	return mixedCredential
}

// responseCredential is the credential resp was answered under, empty
// for no response.
func responseCredential(resp *forgeapi.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Credential
}

// cachedForgeFacts answers keys through cache (resolveCached), keyed by
// forge, host and login: only found and absent facts read under the
// login the call started with are held, for their TTLs. With fresh, held
// facts are read again (factCache.claim).
func (c *Core) cachedForgeFacts(ctx context.Context, cache *factCache[ForgeFact], client *forgeapi.Client, pr PRReference, keys []forgeFactKey, fresh bool, send factSender) []ForgeFact {
	fp, err := client.CredentialFingerprint(ctx)
	if err != nil {
		log.Printf("forge facts: %s %s: credential: %v", pr.Forge, pr.Host, err)
		out := make([]ForgeFact, len(keys))
		for i, k := range keys {
			out[i] = unavailableFact(k.key)
		}
		return out
	}
	prefix := pr.Forge + "\x00" + pr.Host + "\x00" + fp + "\x00"
	credential := ""
	return resolveCached(ctx, cache, c.nowFn, keys, fresh,
		func(k forgeFactKey) string { return prefix + k.key },
		func(k forgeFactKey) string { return k.key },
		func(owned []forgeFactKey) []ForgeFact {
			readCtx, cancel := context.WithTimeout(ctx, forgeFactsTimeout)
			defer cancel()
			var facts []ForgeFact
			facts, credential = send(readCtx, client, pr, owned)
			return facts
		},
		func(fact ForgeFact, readAt time.Time) (bool, time.Time) {
			switch {
			case credential != fp:
				return false, time.Time{}
			case fact.Status == FactFound:
				return true, readAt.Add(forgeFactFoundTTL)
			case fact.Status == FactAbsent:
				return true, readAt.Add(forgeFactAbsentTTL)
			}
			return false, time.Time{}
		})
}

// resolveCached answers one fact per key through cache: a held fact at
// once (with fresh, only one held without expiry: factCache.claim), a key
// another call is reading from that read's answer, and every other key
// from one read of them all, whose facts (one per key, in order) settle
// their flights and are held as hold says. A waiter whose context ends,
// or whose owner's read failed, answers unavailable.
func resolveCached[K any](ctx context.Context, cache *factCache[ForgeFact], now func() time.Time, keys []K, fresh bool,
	cacheKey, factKey func(K) string, read func(owned []K) []ForgeFact, hold func(fact ForgeFact, readAt time.Time) (bool, time.Time)) []ForgeFact {
	out := make([]ForgeFact, len(keys))
	type waiting struct {
		i      int
		flight *factFlight[ForgeFact]
	}
	var waits []waiting
	var owned []K
	var ownedAt []int
	var flights []*factFlight[ForgeFact]
	at := now()
	for i, k := range keys {
		fact, hit, wait, own := cache.claim(cacheKey(k), at, fresh)
		switch {
		case hit:
			out[i] = fact
		case wait != nil:
			waits = append(waits, waiting{i, wait})
		default:
			owned, ownedAt, flights = append(owned, k), append(ownedAt, i), append(flights, own)
		}
	}
	if len(owned) > 0 {
		settled := 0
		defer func() {
			// A read that panicked or answered short leaves flights
			// unanswered; their waiters must not hang.
			for j := settled; j < len(owned); j++ {
				cache.settle(cacheKey(owned[j]), flights[j], ForgeFact{}, false, false, time.Time{})
			}
		}()
		facts := read(owned)
		readAt := now()
		for j, fact := range facts[:min(len(facts), len(owned))] {
			keep, expires := hold(fact, readAt)
			cache.settle(cacheKey(owned[j]), flights[j], fact, true, keep, expires)
			settled = j + 1
			out[ownedAt[j]] = fact
		}
		for j := settled; j < len(owned); j++ {
			out[ownedAt[j]] = unavailableFact(factKey(owned[j]))
		}
	}
	for _, w := range waits {
		fact, ok := w.flight.await(ctx)
		if !ok {
			fact = unavailableFact(factKey(keys[w.i]))
		}
		out[w.i] = fact
	}
	return out
}

// factSlots is a facts read's documents by slot count, smallest first:
// a read of n keys sends the smallest document with at least n slots,
// and more keys than the largest holds are sent in several reads of the
// largest. Each document's unused slots are left out by @include, so the
// forge runs only the used ones.
type factSlots []struct {
	slots int
	doc   forgeapi.Document
}

// pick is the document that holds n keys, n at most the largest's slots.
func (s factSlots) pick(n int) (forgeapi.Document, int) {
	for _, each := range s {
		if each.slots >= n {
			return each.doc, each.slots
		}
	}
	last := s[len(s)-1]
	return last.doc, last.slots
}

func (s factSlots) largest() int { return s[len(s)-1].slots }

// sendFactSlots reads keys in reads of at most slots.largest() keys, each
// through read, which answers one fact per key or the read's error. A
// read the forge refuses as too costly or too slow (tooLarge) is read
// again once, in halves, so a forge that keeps refusing costs at most
// three requests per read. Each failed read is logged once, by operation,
// with no titles: its keys answer unavailable. The credential is the
// reads' (joinCredential).
func sendFactSlots(ctx context.Context, pr PRReference, slots factSlots, keys []forgeFactKey, tooLarge func(error) bool,
	read func(ctx context.Context, doc forgeapi.Document, slots int, keys []forgeFactKey) ([]ForgeFact, *forgeapi.Response, error)) ([]ForgeFact, string) {
	out := make([]ForgeFact, 0, len(keys))
	credential := ""
	var run func(keys []forgeFactKey, depth int)
	run = func(keys []forgeFactKey, depth int) {
		doc, n := slots.pick(len(keys))
		facts, resp, err := read(ctx, doc, n, keys)
		credential = joinCredential(credential, responseCredential(resp))
		if err != nil && tooLarge(err) && len(keys) > 1 && depth == 0 {
			half := len(keys) / 2
			run(keys[:half], depth+1)
			run(keys[half:], depth+1)
			return
		}
		if err != nil {
			log.Printf("forge facts: %s %s %s: %d keys unavailable: %v", pr.Forge, pr.Host, doc.Operation(), len(keys), err)
			for _, k := range keys {
				out = append(out, unavailableFact(k.key))
			}
			return
		}
		out = append(out, facts...)
	}
	for start := 0; start < len(keys); start += slots.largest() {
		run(keys[start:min(start+slots.largest(), len(keys))], 0)
	}
	return out, credential
}

// factAnswer is a GraphQL facts answer split by alias: each alias's data
// and the problems whose path starts at it. An error that is no GraphQL
// answer, an answer without data, or a problem without an alias path
// fails the whole read: no key's answer can be told apart.
type factAnswer struct {
	data     map[string]json.RawMessage
	problems map[string][]forgeapi.GraphQLProblem
}

func newFactAnswer(data map[string]json.RawMessage, err error) (factAnswer, error) {
	answer := factAnswer{data: data}
	if err == nil {
		return answer, nil
	}
	gql, ok := errors.AsType[*forgeapi.GraphQLError](err)
	if !ok || len(gql.Data) == 0 || string(gql.Data) == "null" || gql.Undecoded {
		return factAnswer{}, err
	}
	answer.problems = map[string][]forgeapi.GraphQLProblem{}
	for _, problem := range gql.Problems {
		alias := ""
		if len(problem.Path) > 0 {
			alias, _ = problem.Path[0].(string)
		}
		if alias == "" {
			return factAnswer{}, err
		}
		answer.problems[alias] = append(answer.problems[alias], problem)
	}
	return answer, nil
}

// status is what an alias's problems say of its key: absent when every
// one is NOT_FOUND (a missing node answers null beside one), unavailable
// for any other, "" for none.
func (a factAnswer) status(aliases ...string) string {
	status := ""
	for _, alias := range aliases {
		for _, problem := range a.problems[alias] {
			if problem.Type != "NOT_FOUND" {
				return FactUnavailable
			}
			status = FactAbsent
		}
	}
	return status
}

// decode reads alias's data into out; false when it is null or absent.
func (a factAnswer) decode(alias string, out any) (bool, error) {
	raw, ok := a.data[alias]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, err
	}
	return true, nil
}

// factTitle bounds a title a fact carries.
func factTitle(title string) string {
	return stringsx.ClipRunes(strings.TrimSpace(title), maxFactTitleBytes)
}

// factState is a forge state as a fact says it: open, closed or merged.
func factState(state string) string {
	switch s := NormalizePRState(state); s {
	case "locked":
		return "open"
	default:
		return s
	}
}

// logFactProblems logs, once per read, that its problems made
// unavailable keys unavailable, with the first problem's type and
// message (the forge's, which names no title).
func logFactProblems(pr PRReference, operation string, answer factAnswer, unavailable int) {
	if unavailable == 0 {
		return
	}
	for _, problems := range answer.problems {
		for _, problem := range problems {
			if problem.Type != "NOT_FOUND" {
				log.Printf("forge facts: %s %s %s: %d keys unavailable: %s %s", pr.Forge, pr.Host, operation, unavailable, problem.Type, stringsx.ClipRunes(problem.Message, 200))
				return
			}
		}
	}
}
