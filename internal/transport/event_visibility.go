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
// The rationale for each membership lives on its registry row's Why, and
// the doctrine for each CLASS lives on the Audience constants.
var (
	// loopbackOnlyEventChannels: AudienceLoopbackOnly rows.
	loopbackOnlyEventChannels = make(map[string]bool)
	// remoteOnlyEventChannels: AudienceRemoteOnly rows.
	remoteOnlyEventChannels = make(map[string]bool)
)

func init() {
	for _, policy := range channelPolicies {
		switch policy.Audience {
		case AudienceLoopbackOnly:
			loopbackOnlyEventChannels[policy.Channel] = true
		case AudienceRemoteOnly:
			remoteOnlyEventChannels[policy.Channel] = true
		case AudienceAny:
			// Reaches everyone; in neither filter set.
		}
	}
}

// eventVisibleToOrigin reports whether a channel's frames may reach a
// connection with the given locality. A channel with no registry row is
// visible to everyone — see unregisteredChannelPolicy for the fail-open
// default and the TODO naming the planned fail-closed flip.
func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return !remoteOnlyEventChannels[channel]
	}
	return !loopbackOnlyEventChannels[channel]
}
