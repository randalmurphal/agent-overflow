package workflowapp

// LifecyclePorts contains the one completion policy that still needs root git
// and forge composition. workflowapp owns when it is serialized; the callback
// performs the disposition itself.
type LifecyclePorts struct {
	AutoDispose func(itemID string)
}

// WakeDeliveryPorts is the existing user-message durability ladder expressed
// from the wake consumer's side. QueueMessage's callback fires only once the
// root send path has made the message durable.
type WakeDeliveryPorts struct {
	HasLiveSession func(threadID string) bool
	QueueMessage   func(threadID, message string, onDurable func()) error
	SendMessage    func(threadID, message string) error
}

// AttentionPorts keeps OS notification and optional model-upgraded digest work
// outside workflowapp while preserving their ordering relative to wake work.
type AttentionPorts struct {
	Notify           func(itemID, title, body string) error
	CanUpgradeDigest func() bool
}
