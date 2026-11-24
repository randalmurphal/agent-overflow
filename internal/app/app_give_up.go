package app

import "time"

// A give-up limit bounds how long the app waits for another party (a
// goroutine, a process, a paired computer) before it acts on the missing
// answer: it reports that the party did not answer, or goes on without it.
// Every such limit is read through giveUpBound.

// giveUpPatience, when positive, replaces every give-up limit a test did
// not set itself. Production never writes it. Only the package's TestMain
// may, once, before m.Run: in a test process the other party runs in the
// same process, and a crowded machine must not turn its slowness into the
// limit's failure. A test that needs a limit to fire sets that limit.
var giveUpPatience time.Duration

// giveUpBound is a give-up limit whose product value is product: set when
// a test set it, else giveUpPatience when positive, else product.
func giveUpBound(set, product time.Duration) time.Duration {
	return giveUpBoundWith(set, giveUpPatience, product)
}

// giveUpBoundWith is giveUpBound with the patience given.
func giveUpBoundWith(set, patience, product time.Duration) time.Duration {
	switch {
	case set > 0:
		return set
	case patience > 0:
		return patience
	default:
		return product
	}
}
