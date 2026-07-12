package wsllauncher

import "agent-overflow/internal/notify"

// NotificationActivationQueueCapacity bounds native notification clicks that
// arrive while the WSL backend is still booting. The launcher serializes all
// calls to this queue under its own lifecycle mutex so backend readiness and
// queue state change atomically.
const NotificationActivationQueueCapacity = 8

// NotificationActivationQueue is the launcher's small cold-start FIFO. It
// deliberately contains no goroutines or transport policy: the Windows shell
// owns those, while this cross-platform state seam keeps overflow and drain
// behavior executable in ordinary Go tests.
type NotificationActivationQueue struct {
	items    []notify.Target
	draining bool
}

func NewNotificationActivationQueue() *NotificationActivationQueue {
	return &NotificationActivationQueue{
		items: make([]notify.Target, 0, NotificationActivationQueueCapacity),
	}
}

// Push appends one click, dropping the oldest when the queue is full. start is
// true exactly when the caller should launch the single drain goroutine.
func (q *NotificationActivationQueue) Push(target notify.Target, consumerReady bool) (dropped *notify.Target, start bool) {
	if len(q.items) == NotificationActivationQueueCapacity {
		oldest := q.items[0]
		dropped = &oldest
		copy(q.items, q.items[1:])
		q.items = q.items[:len(q.items)-1]
	}
	q.items = append(q.items, target)
	return dropped, q.startIfReady(consumerReady)
}

// StartIfPending is called when the backend client becomes available.
func (q *NotificationActivationQueue) StartIfPending(consumerReady bool) bool {
	return q.startIfReady(consumerReady)
}

func (q *NotificationActivationQueue) startIfReady(consumerReady bool) bool {
	if !consumerReady || q.draining || len(q.items) == 0 {
		return false
	}
	q.draining = true
	return true
}

// Next returns the next click. An empty queue ends the active drain.
func (q *NotificationActivationQueue) Next() (notify.Target, bool) {
	if len(q.items) == 0 {
		q.draining = false
		return notify.Target{}, false
	}
	target := q.items[0]
	copy(q.items, q.items[1:])
	q.items = q.items[:len(q.items)-1]
	return target, true
}

// Stop ends a drain after launcher/backend cancellation without discarding
// clicks that have not yet been attempted.
func (q *NotificationActivationQueue) Stop() {
	q.draining = false
}
