package transport

var loopbackOnlyEventChannels = map[string]bool{
	"provider:approval":            true,
	"provider:queue_flushed":       true,
	"provider:queue_state_changed": true,
	"provider:user_input":          true,
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return true
	}
	return !loopbackOnlyEventChannels[channel]
}
