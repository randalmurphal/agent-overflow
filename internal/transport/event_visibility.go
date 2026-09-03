package transport

// The two sets below used to be hand-authored maps and were the source of
// truth for who sees a channel. They are now DERIVED from the one authored
// table in event_channels.go — do not add an entry here; add (or
// reclassify) a ChannelPolicy row instead. Their two retention siblings
// (ephemeral / latest-only) are gone entirely: those lookups are cold and
// read the registry directly through channelRetention.
//
// These two survive as maps because eventVisibleToOrigin is the one HOT
// registry consumer — it runs per event per subscriber (EventBus.deliver)
// and again per event per connection (conn.go's event pump). A
// map[string]bool probe is one hash and one byte; going through
// ChannelPolicy would copy two string headers on every delivered frame.
//
// They are keyed by plain string, not eventchan.Channel, for the same
// reason channelPolicyIndex is: the hot path is handed Event.Channel,
// which is the wire spelling — a name a client's replay request may have
// chosen. The one-time init converts the authored rows on the way in;
// string and eventchan.Channel share a representation, so neither that
// conversion nor a caller's costs anything at runtime.
//
// The rationale for each membership lives on its registry row's Why, and
// the doctrine for each CLASS lives on the Audience constants.
var (
	// remoteOnlyEventChannels: AudienceRemoteOnly rows.
	remoteOnlyEventChannels = make(map[string]bool)
	// remoteVisibleEventChannels: rows a NON-loopback connection may
	// receive — AudienceAny and AudienceRemoteOnly. Membership, not
	// absence, grants remote visibility: that is the fail-closed flip. A
	// channel with no registry row is in neither set, so it reaches
	// loopback connections only (see unregisteredChannelPolicy).
	remoteVisibleEventChannels = make(map[string]bool)
)

func init() {
	for _, policy := range channelPolicies {
		switch policy.Audience {
		case AudienceLoopbackOnly:
			// Reaches loopback only; in neither set.
		case AudienceRemoteOnly:
			remoteOnlyEventChannels[string(policy.Channel)] = true
			remoteVisibleEventChannels[string(policy.Channel)] = true
		case AudienceAny:
			remoteVisibleEventChannels[string(policy.Channel)] = true
		}
	}
}

// eventVisibleToOrigin reports whether a channel's frames may reach a
// connection with the given locality. Loopback connections see everything
// except AudienceRemoteOnly rows — including unregistered channels, which
// is what keeps a forgotten registration (and the harness's caller-named
// channels) working locally. Non-loopback connections see only channels a
// registry row explicitly opened to them.
func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return !remoteOnlyEventChannels[channel]
	}
	return remoteVisibleEventChannels[channel]
}

// The scope half of the same question (docs/specs/remote-access.md §5).
//
// eventVisibleToOrigin answers "may a peer at this LOCALITY see this
// channel". It is the whole answer for a connection carrying only the
// launch credential, which names no session and has always been judged by
// where it came from. A connection that named a durable session is judged
// by BOTH: locality first, then the grants that session actually holds.
//
// A frame filter runs per event per subscriber AND per event per
// connection, so the grant test has to cost about what the locality test
// costs. It does: one map probe for the channel's bit and one AND against
// a mask computed once. The mask is a uint32 because there are thirteen
// scope names and a slice scan per frame would be the expensive shape.

// scopeMask is a set of scopes, one bit per index in Scopes.
type scopeMask uint32

// scopeBits indexes a scope to its bit. Built once from Scopes, so a name
// added there joins without a second edit.
var scopeBits = func() map[Scope]scopeMask {
	bits := make(map[Scope]scopeMask, len(Scopes))
	for i, scope := range Scopes {
		bits[scope] = 1 << uint(i)
	}
	return bits
}()

// channelScopeBits is the registry's Scope column as a bit per channel,
// keyed by the wire spelling for the same reason the two sets above are.
// A channel with no row is absent and answers the host bit, which is
// unregisteredChannelPolicy's Scope stated as a mask.
var channelScopeBits = func() map[string]scopeMask {
	bits := make(map[string]scopeMask, len(channelPolicies))
	for _, policy := range channelPolicies {
		bits[string(policy.Channel)] = scopeBits[policy.Scope]
	}
	return bits
}()

// unregisteredChannelBit is what a channel with no row resolves to.
var unregisteredChannelBit = scopeBits[unregisteredChannelPolicy.Scope]

// eventScopeFilter is a connection's answer to "which channels may I
// receive", computed once per connection.
//
// Computed once because a session's grant set cannot change while the
// session lives: nothing updates sessions.scopes, and the one thing that
// does invalidate a session — revocation — force-closes the socket
// through the live-connection registry rather than leaving it streaming.
// A per-frame re-read would buy nothing and cost a store round-trip on
// every event.
//
// The zero value is INACTIVE and admits every channel, which is what a
// connection naming no session must get: unchanged behavior.
type eventScopeFilter struct {
	active  bool
	granted scopeMask
}

// sessionScopeFilter builds the filter for a connection carrying a
// session. hostPresent sets the `host` bit, because host presence is what
// opens a host-scoped channel and no grant ever can — the same rule
// AuthorizeSessionMethod applies to a host-scoped method.
//
// The `session` floor bit is set unconditionally, for the mirror reason:
// this filter is built only for a connection that named one, and the bit
// is not a grant anybody can be given. No channel carries the floor
// today; setting it here is what keeps a row that takes it later from
// silently delivering to nobody.
func sessionScopeFilter(granted []string, hostPresent bool) eventScopeFilter {
	filter := eventScopeFilter{active: true, granted: scopeBits[ScopeSession]}
	for _, name := range granted {
		filter.granted |= scopeBits[Scope(name)]
	}
	if hostPresent {
		filter.granted |= scopeBits[ScopeHost]
	}
	return filter
}

// allows reports whether this connection's grants open the channel.
func (f eventScopeFilter) allows(channel string) bool {
	if !f.active {
		return true
	}
	bit, registered := channelScopeBits[channel]
	if !registered {
		bit = unregisteredChannelBit
	}
	return f.granted&bit != 0
}
