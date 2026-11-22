package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Facts are what a PR body or comment names that only the forge or the
// repository can answer: whether #12 exists and what it is called, a
// GitLab label's colors, an image's size, the lines a permalink points
// at. The review pane asks for every fact of a PR update in one
// ResolveFacts call before any body mounts, so a body paints in its final
// shape. The keys, the found fields of each kind and the statuses are the
// shared contract in docs/architecture/forge-transport.md (Facts).

// MaxFactKeys bounds one ResolveFacts call; the client chunks.
const MaxFactKeys = 256

// The fact statuses. Absent is the owner's answer that the thing does not
// exist; unavailable is that nobody could say (a failed or refused read,
// an object that is not local, a cap, a kind this release does not know).
const (
	FactFound       = "found"
	FactAbsent      = "absent"
	FactUnavailable = "unavailable"
)

// ForgeFacts answers ResolveFacts: one fact per requested key, in the
// request's order.
type ForgeFacts struct {
	Facts []ForgeFact `json:"facts"`
}

// ForgeFact is one key's answer. Every field but Key and Status is a
// found fact's, set by kind and omitted when empty.
type ForgeFact struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	// URL is the canonical web URL on the PR's host.
	URL string `json:"url,omitempty"`
	// Title is an issue, PR or MR title, a user's display name, a commit's
	// subject, or a label's, milestone's, snippet's or epic's title.
	Title string `json:"title,omitempty"`
	// Kind is "issue" or "pull" for ref:, "user", "org" or "group" for
	// user:.
	Kind string `json:"kind,omitempty"`
	// State is "open", "closed" or "merged".
	State string `json:"state,omitempty"`
	// Color and TextColor are a label's background and text, "#rrggbb".
	Color     string `json:"color,omitempty"`
	TextColor string `json:"textColor,omitempty"`
	// OID is an image's or a lines range's blob, or a commit's full id.
	OID string `json:"oid,omitempty"`
	// Size, Width, Height and MIME are an image blob's bytes, natural
	// pixels (zero when unknown) and sniffed type. Fluid marks an image
	// with no natural size and a ratio (an svg with only a viewBox):
	// Width and Height are then the ratio's terms, never pixels
	// (attachment.DisplaySize).
	Size   int64  `json:"size,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Fluid  bool   `json:"fluid,omitempty"`
	MIME   string `json:"mime,omitempty"`
	// LFS marks an image path holding a Git LFS pointer whose object is not
	// in the local LFS store; only OID is set beside it. A pointer whose
	// object is local answers as that image, with the pointer blob's OID.
	LFS bool `json:"lfs,omitempty"`
	// Text is a lines range, "\n" joined without a trailing newline, and
	// Path its repository path.
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
	// Labels is a labels: key's list, the project's labels and its
	// ancestor groups' in the forge's order (by title), and Complete says
	// it holds every one; past maxLabelListPages pages or
	// maxLabelListBytes it is the labels read so far.
	Labels   []ForgeLabel `json:"labels,omitempty"`
	Complete bool         `json:"complete,omitempty"`
}

// ForgeLabel is one label of a labels: list: its title as the forge
// stores it and its colors as "#rrggbb" (empty when the forge's is not
// one).
type ForgeLabel struct {
	Title     string `json:"title"`
	Color     string `json:"color,omitempty"`
	TextColor string `json:"textColor,omitempty"`
}

func unavailableFact(key string) ForgeFact { return ForgeFact{Key: key, Status: FactUnavailable} }
func absentFact(key string) ForgeFact      { return ForgeFact{Key: key, Status: FactAbsent} }

// factKind is a key's kind, the text before its first ':'.
func factKind(key string) string {
	kind, _, _ := strings.Cut(key, ":")
	return kind
}

// The kinds the forge answers and the kinds the repository answers.
var (
	forgeFactKinds = map[string]bool{"ref": true, "mr": true, "user": true, "label": true, "labels": true, "milestone": true, "snippet": true, "epic": true}
	localFactKinds = map[string]bool{"commit": true, "image": true, "lines": true}
)

// ResolveFacts answers keys for the PR pr shows, from pr's forge and from
// the repository at cwd at rev: the forge kinds and the local kinds are
// resolved concurrently, each key once however often it is named, and the
// answer holds one fact per key in keys' order. rev may be zero (no
// loaded revision): the local kinds that name head or base then answer
// unavailable. Every failure to read a fact makes that fact unavailable
// and is logged by its reader; the call fails only on arguments it cannot
// run with. With fresh (the person reloading the PR) a held fact that can
// change is read again, not answered from the caches; a fact held for
// good (a local one named by a full object id) still answers from them,
// and a fresh read that fails leaves the held fact for later calls.
func (c *Core) ResolveFacts(ctx context.Context, cwd string, pr PRReference, rev PRRevision, keys []string, fresh bool) (ForgeFacts, error) {
	if len(keys) > MaxFactKeys {
		return ForgeFacts{}, fmt.Errorf("resolve facts: %d keys exceeds %d", len(keys), MaxFactKeys)
	}
	if err := pr.Validate(); err != nil {
		return ForgeFacts{}, fmt.Errorf("resolve facts: %w", err)
	}
	for _, sha := range []string{rev.HeadSHA, rev.BaseSHA} {
		if sha != "" && !treeOIDPattern.MatchString(sha) {
			return ForgeFacts{}, fmt.Errorf("resolve facts: commit %q is not a full object ID", sha)
		}
	}
	facts := make([]ForgeFact, len(keys))
	first := make(map[string]int, len(keys))
	var forgeKeys, localKeys []string
	for i, key := range keys {
		if j, seen := first[key]; seen {
			facts[i].Key = keys[j]
			continue
		}
		first[key] = i
		switch kind := factKind(key); {
		case forgeFactKinds[kind]:
			forgeKeys = append(forgeKeys, key)
		case localFactKinds[kind]:
			localKeys = append(localKeys, key)
		default:
			facts[i] = unavailableFact(key)
		}
	}
	var forgeFacts []ForgeFact
	var wg sync.WaitGroup
	if len(forgeKeys) > 0 {
		wg.Go(func() { forgeFacts = c.resolveForgeFacts(ctx, pr, forgeKeys, fresh) })
	}
	localFacts := c.resolveLocalFacts(ctx, cwd, rev, localKeys, fresh)
	wg.Wait()
	for _, fact := range append(forgeFacts, localFacts...) {
		facts[first[fact.Key]] = fact
	}
	for i := range facts {
		if facts[i].Status == "" {
			facts[i] = facts[first[facts[i].Key]]
		}
	}
	return ForgeFacts{Facts: facts}, nil
}

// factResolver is a forge adapter that answers forge facts: one fact per
// key, in keys' order, read again past its cache with fresh.
type factResolver interface {
	resolveFacts(ctx context.Context, pr PRReference, keys []forgeFactKey, fresh bool) []ForgeFact
}

// resolveForgeFacts answers keys, each a forge kind's, from pr's forge.
// A key that does not parse, or names a kind pr's forge has not, answers
// as forgeFactKey says without a request.
func (c *Core) resolveForgeFacts(ctx context.Context, pr PRReference, keys []string, fresh bool) []ForgeFact {
	out := make([]ForgeFact, 0, len(keys))
	parsed := make([]forgeFactKey, 0, len(keys))
	others := map[string]bool{}
	own := strings.ToLower(pr.Project())
	for _, key := range keys {
		k, status := parseForgeFactKey(pr.Forge, key)
		if status != "" {
			out = append(out, ForgeFact{Key: key, Status: status})
			continue
		}
		if k.namespace != "" && strings.ToLower(k.namespace) != own {
			others[strings.ToLower(k.namespace)] = true
			if len(others) > maxFactOtherProjects {
				// A fifth other project and later ones: one request stays
				// bounded to the PR's project and four others.
				delete(others, strings.ToLower(k.namespace))
				out = append(out, unavailableFact(key))
				continue
			}
		}
		parsed = append(parsed, k)
	}
	if len(parsed) == 0 {
		return out
	}
	resolver, ok := c.forges[pr.Forge].(factResolver)
	if !ok {
		for _, k := range parsed {
			out = append(out, unavailableFact(k.key))
		}
		return out
	}
	return append(out, resolver.resolveFacts(ctx, pr, parsed, fresh)...)
}

// maxFactOtherProjects is how many projects (or groups) other than the
// PR's own one call's forge keys may name; keys of more answer
// unavailable.
const maxFactOtherProjects = 4

// forgeFactKey is a parsed forge key. namespace is the project or group
// it is read in (empty for user:), number an issue, MR, snippet or epic
// number, and name a label or milestone title or a user or group path.
type forgeFactKey struct {
	key       string
	kind      string
	namespace string
	number    int
	name      string
}

// parseForgeFactKey reads key for forge. A key whose kind forge does not
// have, or that does not parse, answers unavailable; a well-formed key
// naming a project or user the forge cannot hold (an invalid path or
// login) answers absent. Both come back as the status, with no request.
func parseForgeFactKey(forge, key string) (forgeFactKey, string) {
	kind, rest, _ := strings.Cut(key, ":")
	k := forgeFactKey{key: key, kind: kind}
	if !forgeFactKindOn(forge, kind) {
		return k, FactUnavailable
	}
	numbered := func(sep string) string {
		i := strings.LastIndex(rest, sep)
		if i <= 0 {
			return FactUnavailable
		}
		n, ok := factNumber(rest[i+1:])
		if !ok {
			return FactUnavailable
		}
		k.namespace, k.number = rest[:i], n
		return ""
	}
	titled := func(sep string) string {
		i := strings.Index(rest, sep)
		if i <= 0 || i == len(rest)-1 {
			return FactUnavailable
		}
		k.namespace, k.name = rest[:i], rest[i+1:]
		if len(k.name) > maxFactNameBytes {
			return FactUnavailable
		}
		return ""
	}
	var status string
	switch kind {
	case "ref":
		status = numbered("#")
	case "mr":
		status = numbered("!")
	case "snippet":
		status = numbered("$")
	case "epic":
		status = numbered("&")
	case "label":
		status = titled("~")
	case "labels":
		if rest == "" {
			return k, FactUnavailable
		}
		k.namespace = rest
	case "milestone":
		status = titled("%")
	case "user":
		if rest == "" || len(rest) > maxFactNameBytes {
			return k, FactUnavailable
		}
		k.name = rest
		if !validForgeAccount(forge, rest) {
			return k, FactAbsent
		}
		return k, ""
	}
	if status != "" {
		return k, status
	}
	if kind == "epic" {
		if !validGroupPath(k.namespace) {
			return k, FactAbsent
		}
		return k, ""
	}
	if _, _, err := SplitProjectForForge(forge, k.namespace); err != nil {
		return k, FactAbsent
	}
	return k, ""
}

// maxFactNameBytes bounds a title or account name in a key; forge titles
// are at most 255 characters.
const maxFactNameBytes = 1024

// forgeFactKindOn is whether forge has kind: GitHub answers ref: and
// user:, GitLab every forge kind.
func forgeFactKindOn(forge, kind string) bool {
	switch forge {
	case "github":
		return kind == "ref" || kind == "user"
	case "gitlab":
		return forgeFactKinds[kind]
	}
	return false
}

// factNumber reads a positive decimal without leading zeros that fits an
// int32, the forges' number type.
func factNumber(s string) (int, bool) {
	if s == "" || s[0] == '0' || len(s) > 10 {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int(n), true
}

// validForgeAccount is whether name can be a GitHub login (letters,
// digits and single hyphens, at most 39) or a GitLab user or group path
// (project path segments).
func validForgeAccount(forge, name string) bool {
	if forge == "github" {
		if len(name) > 39 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
			return false
		}
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			default:
				return false
			}
		}
		return true
	}
	return validGroupPath(name)
}

func validGroupPath(path string) bool {
	if path == "" {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if ValidateProjectSegment(segment) != nil {
			return false
		}
	}
	return true
}
