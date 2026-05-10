package transport

var loopbackOnlyEventChannels = map[string]bool{
	"provider:approval":            true,
	"provider:queue_flushed":       true,
	"provider:queue_state_changed": true,
	"provider:user_input":          true,
	// provider:account carries the user's authenticated subscriptionType
	// + tokenSource (oauth | apikey | console) + apiProvider. tokenSource
	// is upstream-typed and discloses the auth model of the local user;
	// subscriptionType + apiProvider are plan/billing identity. Combined,
	// this is single-frame profiling disclosure to a LAN-attached
	// subscriber. Loopback-only.
	"provider:account": true,
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return true
	}
	return !loopbackOnlyEventChannels[channel]
}
