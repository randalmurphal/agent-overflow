package transport

var loopbackOnlyEventChannels = map[string]bool{
	"git:status":                     true,
	"provider:approval":              true,
	"provider:status":                true,
	"provider:queue_flushed":         true,
	"provider:queue_restored":        true,
	"provider:queue_state_changed":   true,
	"provider:background_task_state": true,
	"provider:user_input":            true,
	"terminal:exit":                  true,
	"terminal:output":                true,
	// provider:account carries the user's authenticated subscriptionType
	// + tokenSource (oauth | apikey | console) + apiProvider. tokenSource
	// is upstream-typed and discloses the auth model of the local user;
	// subscriptionType + apiProvider are plan/billing identity. Combined,
	// this is single-frame profiling disclosure to a LAN-attached
	// subscriber. Loopback-only.
	"provider:account": true,

	// provider:usage (token counts, context %, rate limits) and
	// provider:session_died are open to remote clients — essential
	// feedback for understanding resource consumption and provider
	// crashes. Without session_died, a remote viewer sees the turn
	// silently stop with no explanation.
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return true
	}
	return !loopbackOnlyEventChannels[channel]
}
