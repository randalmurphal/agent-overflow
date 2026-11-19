package workflowapp

import "agent-overflow/internal/workflow/wake"

// LifecyclePorts contains the one completion policy that still needs root git
// and forge composition. workflowapp owns when it is serialized; the callback
// performs the disposition itself.
type LifecyclePorts struct {
	AutoDispose func(itemID string)
}

// WakeDeliveryPorts is the existing user-message durability ladder expressed
// from the wake consumer's side.
type WakeDeliveryPorts struct {
	// QueueMessage queues a run's wake for the thread, attributed to the
	// workflow it came from, and starts the thread's session for it when it
	// has none. onDurable fires only once the message reaches the provider
	// or the composer.
	QueueMessage func(threadID, workflowID string, message wake.Message, onDurable func()) error
}

// AttentionPorts keeps OS notification and optional model-upgraded digest work
// outside workflowapp while preserving their ordering relative to wake work.
type AttentionPorts struct {
	Notify           func(itemID, title, body string) error
	CanUpgradeDigest func() bool
}
