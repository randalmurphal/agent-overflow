package accountswitch

import (
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// Strategy values of the accountAutoSwitchStrategy setting.
const (
	StrategySoonestReset = "soonest-reset"
	StrategyAccountOrder = "account-order"
)

// Reason says why a switch happened.
type Reason string

const (
	ReasonExhausted Reason = "exhausted"
	ReasonThreshold Reason = "threshold"
	ReasonRefused   Reason = "refused"
	ReasonOrder     Reason = "order"
)

const (
	providerClaude = "claude"
	providerCodex  = "codex"

	// noReadingMarkDuration bounds a refusal mark when no limit names a reset.
	noReadingMarkDuration = time.Hour
)

type state int

const (
	stateClear state = iota
	statePastThreshold
	stateExhausted
)

// providerWide reports whether entry applies to every model of the usage
// family. Twin of defaultLimit in frontend/src/lib/stores/rateLimitsInfo.svelte.ts.
func providerWide(prov string, entry provider.RateLimitEntry) bool {
	id := strings.ToLower(strings.TrimSpace(entry.LimitID))
	switch prov {
	case providerCodex:
		return id == "codex"
	case providerClaude:
		return id == "session" || id == "weekly_all" || id == "five_hour" || id == "seven_day"
	}
	return false
}

// modelKey is the normalized, lowercase model slug used to count demand.
func modelKey(prov, model string) string {
	model = strings.TrimSpace(model)
	if prov == providerClaude {
		model = provider.NormalizeModelSlug(string(provider.Claude), model)
	}
	return strings.ToLower(model)
}

// modelFamily maps a model key onto the name scoped limits use for it: the
// token after `claude-` for Claude, the whole slug for Codex.
func modelFamily(prov, key string) string {
	if key == "" {
		return ""
	}
	if prov == providerClaude {
		return claudeFamilyToken(key)
	}
	return key
}

func claudeFamilyToken(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "claude-")
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	return s
}

// bucketFamilies lists the model families a scoped entry names. Claude names
// the family in the LimitID suffix (`weekly_scoped:fable`, possibly
// `weekly_scoped:claude-fable:2`) and in LimitName ("Fable"). Codex limit IDs
// are unstable, so only the slugified LimitName counts.
func bucketFamilies(prov string, entry provider.RateLimitEntry) []string {
	var out []string
	add := func(f string) {
		if f != "" && (len(out) == 0 || out[0] != f) {
			out = append(out, f)
		}
	}
	switch prov {
	case providerClaude:
		id := strings.ToLower(strings.TrimSpace(entry.LimitID))
		if i := strings.IndexByte(id, ':'); i >= 0 {
			suffix := id[i+1:]
			if j := strings.IndexByte(suffix, ':'); j >= 0 {
				suffix = suffix[:j]
			}
			add(claudeFamilyToken(suffix))
		}
		add(claudeFamilyToken(slugify(entry.LimitName)))
	case providerCodex:
		add(slugify(entry.LimitName))
	}
	return out
}

// slugify lowercases s and joins its runs of letters, digits and dots with
// single dashes.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

// applies reports whether entry limits a model of family. An empty family
// has only provider-wide limits, since bucketFamilies never names "".
func applies(prov string, entry provider.RateLimitEntry, family string) bool {
	if providerWide(prov, entry) {
		return true
	}
	for _, f := range bucketFamilies(prov, entry) {
		if f == family {
			return true
		}
	}
	return false
}

// expired reports whether entry's reset time has passed.
func expired(entry provider.RateLimitEntry, now time.Time) bool {
	return entry.ResetsAt > 0 && entry.ResetsAt <= now.Unix()
}

// effectivePercent counts an entry whose reset time has passed as 0%.
func effectivePercent(entry provider.RateLimitEntry, now time.Time) float64 {
	if expired(entry, now) {
		return 0
	}
	return entry.UsedPercent
}

// mark records that a provider refused a turn of family on one account.
type mark struct {
	family    string
	allModels bool
	until     time.Time
	limit     provider.RateLimitEntry
	hasLimit  bool
	// guessed is true when no limit named a future reset, so until is the
	// one-hour fallback.
	guessed bool
}

func (m mark) names(family string) bool {
	return m.allModels || m.family == family
}

// newMark builds a refusal mark for family from the account's reading. The
// mark lasts until the reset of the most used limit that applies, else of the
// most used limit of any kind, else one hour; a limit without a future reset
// time also gives one hour. A provider-wide limit past the threshold widens
// the mark to every model. Among equally used limits the later reset wins.
func newMark(prov, family string, limits []provider.RateLimitEntry, threshold int, now time.Time) mark {
	m := mark{family: family, until: now.Add(noReadingMarkDuration), guessed: true}
	best, ok := mostUsed(limits, now, func(e provider.RateLimitEntry) bool { return applies(prov, e, family) })
	if !ok {
		best, ok = mostUsed(limits, now, func(provider.RateLimitEntry) bool { return true })
	}
	if !ok {
		return m
	}
	m.limit, m.hasLimit = best, true
	if best.ResetsAt > now.Unix() {
		m.until, m.guessed = time.Unix(best.ResetsAt, 0), false
	}
	if providerWide(prov, best) && effectivePercent(best, now) >= float64(threshold) {
		m.allModels = true
	}
	return m
}

func mostUsed(limits []provider.RateLimitEntry, now time.Time, keep func(provider.RateLimitEntry) bool) (provider.RateLimitEntry, bool) {
	var best provider.RateLimitEntry
	found := false
	for _, e := range limits {
		if !keep(e) {
			continue
		}
		p := effectivePercent(e, now)
		bp := effectivePercent(best, now)
		if !found || p > bp || p == bp && e.ResetsAt > best.ResetsAt {
			best, found = e, true
		}
	}
	return best, found
}

// assessment is an account's state for one model family.
type assessment struct {
	state    state
	refused  bool // exhausted by a refusal mark rather than a reading
	limit    provider.RateLimitEntry
	hasLimit bool
	percent  float64
	// serveAt is the earliest time the account can serve the family again
	// when exhausted; zero when unknown.
	serveAt time.Time
}

// assess classifies an account for family. With confirm nil, a passed reset
// or mark expiry counts at once. Otherwise it counts only when confirm
// accepts that time; the continue check uses this to wait for a reading.
func assess(prov, family string, limits []provider.RateLimitEntry, marks []mark, threshold int, now time.Time, confirm func(time.Time) bool) assessment {
	var a assessment
	var latestReset time.Time
	resetUnknown := false
	for _, e := range limits {
		if !applies(prov, e, family) {
			continue
		}
		p := e.UsedPercent
		if expired(e, now) && (confirm == nil || confirm(time.Unix(e.ResetsAt, 0))) {
			p = 0
		}
		if !a.hasLimit || p > a.percent || p == a.percent && e.ResetsAt > a.limit.ResetsAt {
			a.limit, a.hasLimit, a.percent = e, true, p
		}
		if p >= 100 {
			if e.ResetsAt <= 0 {
				resetUnknown = true
			} else if r := time.Unix(e.ResetsAt, 0); r.After(latestReset) {
				latestReset = r
			}
		}
	}
	marked := false
	for _, m := range marks {
		if !m.names(family) {
			continue
		}
		if now.Before(m.until) || confirm != nil && !confirm(m.until) {
			marked = true
			if m.until.After(latestReset) {
				latestReset = m.until
			}
		}
	}
	switch {
	case a.hasLimit && a.percent >= 100:
		a.state = stateExhausted
	case marked:
		a.state, a.refused = stateExhausted, true
	case a.hasLimit && a.percent >= float64(threshold):
		a.state = statePastThreshold
	}
	if a.state == stateExhausted && !resetUnknown {
		a.serveAt = latestReset
	}
	return a
}

// demandModel is one distinct model in the demand.
type demandModel struct {
	key    string
	family string
}

func buildDemand(prov string, models []string) []demandModel {
	out := make([]demandModel, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		key := modelKey(prov, m)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, demandModel{key: key, family: modelFamily(prov, key)})
	}
	return out
}

type score struct {
	exhausted, past int
}

func (s score) less(o score) bool {
	if s.exhausted != o.exhausted {
		return s.exhausted < o.exhausted
	}
	return s.past < o.past
}

func (s score) clear() bool { return s.exhausted == 0 && s.past == 0 }

// candidate is one listed account with what the decision needs about it.
type candidate struct {
	Account
	limits []provider.RateLimitEntry
	marks  []mark
	// usable is false when the account needs a sign-in, by its listing or
	// by a switch that reported it.
	usable bool
}

type decideInput struct {
	provider   string
	selectedID string
	pinned     bool
	accounts   []candidate // card order
	demand     []demandModel
	strategy   string
	threshold  int
	allowOrder bool
	now        time.Time
}

// decision is a chosen switch. A zero decision (to == "") means stay.
type decision struct {
	from, to Account
	reason   Reason
	model    string // demand key of the model that blocked the selected account
	limit    provider.RateLimitEntry
	percent  float64
	hasLimit bool
}

func (d decision) switching() bool { return d.to.ID != "" }

func scoreFor(in decideInput, c candidate) score {
	var s score
	for _, d := range in.demand {
		switch assess(in.provider, d.family, c.limits, c.marks, in.threshold, in.now, nil).state {
		case stateExhausted:
			s.exhausted++
		case statePastThreshold:
			s.past++
		}
	}
	return s
}

// decide applies "Choosing an account". It switches only when the selected
// account is in the pool and another usable pool account exists.
func decide(in decideInput) decision {
	sel := -1
	others := 0
	for i, c := range in.accounts {
		if c.ID == in.selectedID {
			sel = i
		} else if c.InPool && c.usable {
			others++
		}
	}
	if sel < 0 || !in.accounts[sel].InPool || others == 0 {
		return decision{}
	}
	order := poolOrder(in.provider, in.strategy, in.accounts, in.now)
	selected := in.accounts[sel]
	selScore := scoreFor(in, selected)

	bestIdx := -1
	var bestScore score
	for _, i := range order {
		c := in.accounts[i]
		if i == sel || !c.usable {
			continue
		}
		s := scoreFor(in, c)
		if bestIdx < 0 || s.less(bestScore) {
			bestIdx, bestScore = i, s
		}
	}
	if bestScore.less(selScore) {
		d := decision{from: selected.Account, to: in.accounts[bestIdx].Account}
		blockedBy(in, selected, &d)
		return d
	}
	if !in.allowOrder || in.pinned || !selScore.clear() {
		return decision{}
	}
	for _, i := range order {
		if i == sel {
			return decision{}
		}
		c := in.accounts[i]
		if c.usable && scoreFor(in, c).clear() {
			return decision{from: selected.Account, to: c.Account, reason: ReasonOrder}
		}
	}
	return decision{}
}

// blockedBy fills the reason and cause from the demanded model the selected
// account is worst for; the first such model in demand order wins ties.
func blockedBy(in decideInput, selected candidate, d *decision) {
	var worst assessment
	found := false
	for _, dm := range in.demand {
		a := assess(in.provider, dm.family, selected.limits, selected.marks, in.threshold, in.now, nil)
		if !found || a.state > worst.state {
			worst, found, d.model = a, true, dm.key
		}
	}
	switch {
	case worst.state == stateExhausted && worst.refused:
		d.reason = ReasonRefused
	case worst.state == stateExhausted:
		d.reason = ReasonExhausted
	default:
		d.reason = ReasonThreshold
	}
	if worst.hasLimit && !worst.refused {
		d.limit, d.percent, d.hasLimit = worst.limit, worst.percent, true
	}
	if worst.refused {
		for _, m := range selected.marks {
			if m.names(in.demandFamily(d.model)) && in.now.Before(m.until) && m.hasLimit {
				d.limit, d.percent, d.hasLimit = m.limit, effectivePercent(m.limit, in.now), true
				break
			}
		}
	}
}

func (in decideInput) demandFamily(key string) string {
	for _, d := range in.demand {
		if d.key == key {
			return d.family
		}
	}
	return ""
}

// poolOrder returns the indexes of the pool accounts in the order the
// strategy setting gives them, the one an order switch follows. soonest-reset sorts by the reset of each account's longest provider-wide
// window; a window that has not started or has already reset sorts last.
// Ties and account-order keep card order.
func poolOrder(prov, strategy string, accounts []candidate, now time.Time) []int {
	order := make([]int, 0, len(accounts))
	for i, c := range accounts {
		if c.InPool {
			order = append(order, i)
		}
	}
	if strategy == StrategyAccountOrder {
		return order
	}
	resets := make(map[int]int64, len(order))
	for _, i := range order {
		resets[i] = longestWindowReset(prov, accounts[i].limits, now)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := resets[order[a]], resets[order[b]]
		if ra == 0 || rb == 0 {
			return ra != 0 && rb == 0
		}
		return ra < rb
	})
	return order
}

// longestWindowReset is the reset time of the provider-wide entry with the
// largest window, or 0 when that window has not started or has reset.
func longestWindowReset(prov string, limits []provider.RateLimitEntry, now time.Time) int64 {
	var longest provider.RateLimitEntry
	found := false
	for _, e := range limits {
		if !providerWide(prov, e) {
			continue
		}
		if !found || e.WindowMins > longest.WindowMins {
			longest, found = e, true
		}
	}
	if !found || longest.ResetsAt <= 0 || expired(longest, now) {
		return 0
	}
	return longest.ResetsAt
}

// earliestServe returns the earliest time one of accounts can serve family
// again and that account. ok is false when none of them reports a time.
func earliestServe(prov, family string, accounts []candidate, threshold int, now time.Time) (time.Time, Account, bool) {
	var at time.Time
	var who Account
	ok := false
	for _, c := range accounts {
		a := assess(prov, family, c.limits, c.marks, threshold, now, nil)
		if a.state != stateExhausted || a.serveAt.IsZero() {
			continue
		}
		if !ok || a.serveAt.Before(at) {
			at, who, ok = a.serveAt, c.Account, true
		}
	}
	return at, who, ok
}
