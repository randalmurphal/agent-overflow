package trackerfake

import (
	"bytes"
	"cmp"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"maps"
	"mime/multipart"
	"slices"
	"strings"
)

// SetToken replaces a tenant's credential, so a test rotates it while
// clients hold the old one: a Jira site's API token (tenant is the
// site), a Tempo account's bearer token (tenant is its Jira site), or a
// Linear workspace's API key (tenant is its URL key). The next request
// with the old credential is refused as the tracker refuses it.
func (e *Engine) SetToken(t Tracker, tenant, token string) error {
	if err := checkToken("set token: the token", token); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch t {
	case TrackerJira, TrackerTempo:
		s := e.jira[tenant]
		switch {
		case s == nil:
			return fmt.Errorf("trackerfake: set token: no Jira site %s", tenant)
		case t == TrackerJira:
			s.token = token
			return nil
		case s.Tempo == nil:
			return fmt.Errorf("trackerfake: set token: Jira site %s has no Tempo account", tenant)
		}
		if other := e.tempoTenant(token); other != nil && other != s {
			return fmt.Errorf("trackerfake: set token: the Tempo token is %s's already", other.Site)
		}
		s.tempoToken = token
		return nil
	case TrackerLinear:
		w := e.linear[tenant]
		if w == nil {
			return fmt.Errorf("trackerfake: set token: no Linear workspace %s", tenant)
		}
		if other := e.linearTenant(token); other != nil && other != w {
			return fmt.Errorf("trackerfake: set token: the Linear key is workspace %s's already", other.key())
		}
		w.apiKey = token
		return nil
	}
	return fmt.Errorf("trackerfake: set token: unknown tracker %q", t)
}

// checkTokens refuses a seed whose Tempo tokens or Linear keys would name
// two tenants, since those credentials are how the shared origins tell
// tenants apart. Callers hold mu.
func (e *Engine) checkTokens(sites []*jiraSite, spaces []*linearWorkspace) error {
	finalSites := maps.Clone(e.jira)
	for _, s := range sites {
		finalSites[s.Site] = s
	}
	tempo := map[string]string{}
	for _, s := range sortedValues(finalSites) {
		if s.Tempo == nil {
			continue
		}
		if other, ok := tempo[s.tempoToken]; ok {
			return fmt.Errorf("trackerfake: sites %s and %s share a Tempo token", other, s.Site)
		}
		tempo[s.tempoToken] = s.Site
	}
	finalSpaces := maps.Clone(e.linear)
	for _, w := range spaces {
		finalSpaces[w.key()] = w
	}
	keys := map[string]string{}
	for _, w := range sortedValues(finalSpaces) {
		if other, ok := keys[w.apiKey]; ok {
			return fmt.Errorf("trackerfake: Linear workspaces %s and %s share an API key", other, w.key())
		}
		keys[w.apiKey] = w.key()
	}
	return nil
}

// minTokenLength is the shortest credential the engine takes, so that
// replacing every credential in logged text never replaces common text.
const minTokenLength = 8

func checkToken(what, token string) error {
	if len(token) < minTokenLength {
		return fmt.Errorf("trackerfake: %s is shorter than %d characters", what, minTokenLength)
	}
	return nil
}

// tempoTenant is the site whose Tempo account token names. Callers hold
// mu.
func (e *Engine) tempoTenant(token string) *jiraSite {
	for _, s := range e.jira {
		if s.Tempo != nil && sameSecret(s.tempoToken, token) {
			return s
		}
	}
	return nil
}

// linearTenant is the workspace whose API key key is. Callers hold mu.
func (e *Engine) linearTenant(key string) *linearWorkspace {
	for _, w := range e.linear {
		if sameSecret(w.apiKey, key) {
			return w
		}
	}
	return nil
}

func sameSecret(want, got string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// presentedSecrets are every form of the credentials Authorization
// values present: each whole value (a Linear API key), the credential
// after its scheme (a bearer token, a Basic pair's encoding), and a Basic
// pair decoded, whole and its token. serve takes them when a request
// arrives, before any tenant lookup, refusal or route can log one.
func presentedSecrets(values []string) []string {
	var out []string
	for _, v := range values {
		out = append(out, v)
		scheme, cred, ok := strings.Cut(strings.TrimSpace(v), " ")
		if !ok {
			continue
		}
		cred = strings.TrimSpace(cred)
		out = append(out, cred)
		if !strings.EqualFold(scheme, "Basic") {
			continue
		}
		if pair, err := base64.StdEncoding.DecodeString(cred); err == nil {
			_, token, _ := strings.Cut(string(pair), ":")
			out = append(out, string(pair), token)
		}
	}
	return out
}

// secrets are every credential the engine holds and those x presented,
// which no logged text may repeat. Callers hold mu.
func (e *Engine) secrets(x *exchange) []string {
	out := slices.Clone(x.secrets)
	for _, s := range e.jira {
		out = append(out, s.token, s.tempoToken)
	}
	for _, w := range e.linear {
		out = append(out, w.apiKey)
	}
	// Longest first, so a secret that holds another is replaced whole.
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

// multipartNames are the file names of a multipart body's parts.
func multipartNames(body []byte, boundary string) []string {
	if boundary == "" {
		return nil
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	var out []string
	for {
		p, err := r.NextPart()
		if err != nil {
			return out
		}
		if name := p.FileName(); name != "" {
			out = append(out, name)
		}
	}
}

// sortedValues are m's values in key order.
func sortedValues[V any](m map[string]V) []V {
	keys := slices.Sorted(maps.Keys(m))
	out := make([]V, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}
