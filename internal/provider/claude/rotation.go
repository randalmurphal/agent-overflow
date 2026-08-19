package claude

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"log"
	"time"
)

// refreshBuffer mirrors the CLI's own proactive-refresh window, so this side
// arms on exactly the credentials that make the CLI rotate. Verified against
// the 2.1.234 bundle, whose predicate is
//
//	function VAe(e){ if(e===null) return !1; let t=300000; return Date.now()+t>=e }
//
// — five minutes, boundary inclusive, with a null expiry meaning "no refresh",
// which is what CredentialExpired answers for an absent or unreadable one.
const refreshBuffer = 5 * time.Minute

// rotationSettleTimeout bounds the wait for an expected rotation to become
// durable. The wait ends the instant the credential changes, so this is only
// ever paid in full when the refresh failed without writing anything.
//
// Sized off what the CLI still has to do after the token endpoint answers: a
// profile round-trip, then a `.storage-write` lock whose retry ladder alone
// can run several seconds. It is deliberately NOT larger than that — the probe
// runs under a mutex that user-visible operations queue behind, and the
// front-end RPC gives up at 60s.
const rotationSettleTimeout = 10 * time.Second

// The poll interval grows so the common case (the write lands within a couple
// of hundred milliseconds) costs two or three reads, while a full timeout
// costs a couple of dozen rather than a couple of hundred — reading the
// credential is a Keychain subprocess on macOS, not a file read.
const (
	rotationSettleFirstPoll = 50 * time.Millisecond
	rotationSettleMaxPoll   = time.Second
	rotationSettlePollGrow  = 1.5
)

// rotationWatch keeps a probe's CLI alive until a token rotation the probe
// itself triggered has reached durable storage.
//
// The CLI fires its OAuth refresh at startup as a DETACHED task and answers
// the `initialize` control_request from cached local state without awaiting it
// (verified against 2.1.234). Anthropic's token endpoint retires the previous
// refresh token the moment it processes the request, and the CLI still has a
// profile round-trip, a `~/.claude.json` write, and a `.storage-write` lock
// acquisition to get through before the replacement pair reaches disk — with
// no exit handler, no fsync, and no recovery if the process dies first. Under
// `--max-turns 0` the CLI exits on stdin EOF, so closing the probe's stdin the
// moment its answer arrives kills it inside that window.
//
// Spike-measured 2026-08-18 against a fake token endpoint: the rotation
// commits ~94ms BEFORE the initialize response the probe keys off, the
// credential lands ~56ms after it, and the CLI exits ~32ms after stdin close.
// The rotation was lost in 9 of 11 trials of the app's exact teardown, and in
// every teardown variant including the graceful baseline — closing stdin at
// all is what kills it, so the SIGTERM/SIGKILL ladder never comes into play.
// A lost rotation is a permanently dead login: what stays on disk is a refresh
// token the server has already retired, and the next use answers
// invalid_grant, which makes the CLI blank the credential in place.
//
// A fixed delay is NOT a fix — the spike measured the safe delay scaling with
// network latency (200ms survived 8/8 at zero RTT and failed 3/3 at a
// realistic one). The wait has to be driven by the credential actually
// changing, which is what baseline below is for.
type rotationWatch struct {
	read   func() ([]byte, error)
	digest [sha256.Size]byte
	armed  bool
	// baseline records that digest describes the credential as it stood
	// before the CLI started. Without one the watch is BLIND: it still holds
	// the process for the full budget, because "we could not observe the
	// credential" is not evidence that there is nothing to protect, but it
	// cannot end early.
	baseline bool
}

// armRotationWatch decides, BEFORE the CLI is spawned, whether this invocation
// is going to rotate the credential, and records what the credential looked
// like beforehand.
//
// expected is the caller's own knowledge that a rotation is coming, and it
// OVERRIDES the expiry reading. The expiry check catches Claude's proactive
// refresh; it cannot catch the forced one. In 2.1.234 the refresh entry point
// takes a force flag that skips every expiry gate (`if(!t)`-guarded), and the
// OAuth 401 recovery path calls it with force set — so a credential whose
// stored expiry is hours away still rotates once the server rejects its
// bearer. The one caller that spawns the CLI after a 401 knows this and says
// so, rather than this code guessing from bytes that look fine.
//
// Reads are classified rather than lumped together, because "there is no
// credential" and "I could not read the credential" have opposite safe
// answers:
//
//   - absent — nothing to rotate, so no wait. This is the common cheap case
//     (an unauthenticated host), and arming it would tax every probe.
//   - unreadable — a transient Keychain failure, a denied access prompt. The
//     credential may well exist and be rotating right now, so the watch arms
//     BLIND rather than silently switching the protection off.
//
// A nil reader disarms: with no way to observe the credential there is nothing
// to hold for, and TestClaudeProbeConfigAlwaysWiresTheRotationReader keeps
// production from reaching this case by accident.
func armRotationWatch(read func() ([]byte, error), expected bool, now time.Time) rotationWatch {
	if read == nil {
		return rotationWatch{}
	}
	data, err := read()
	switch {
	case err == nil:
		if !expected && !CredentialExpired(data, now.Add(refreshBuffer)) {
			return rotationWatch{read: read}
		}
		return rotationWatch{
			read:     read,
			digest:   sha256.Sum256(data),
			armed:    true,
			baseline: true,
		}
	case errors.Is(err, fs.ErrNotExist):
		return rotationWatch{read: read}
	default:
		log.Printf(
			"claude: probe cannot read the credential (%v); holding the CLI open for a "+
				"possible token rotation it cannot observe",
			err,
		)
		return rotationWatch{read: read, armed: true}
	}
}

// budget is the extra process lifetime this watch needs beyond the probe's own
// deadline. Zero when no rotation is expected, which keeps every ordinary
// probe on exactly the timings it had before.
func (w rotationWatch) budget() time.Duration {
	if !w.armed {
		return 0
	}
	return rotationSettleTimeout
}

// settle blocks until the expected rotation has been written or the context
// expires. It must run BEFORE the process is torn down — closing stdin is what
// kills the CLI, so a settle after teardown protects nothing.
//
// A read failure inside the loop is NOT a reason to stop waiting. It says the
// credential is unreadable right now, which is exactly when the CLI is most
// likely to be mid-write; returning on it would restore the original bug with
// the protection appearing to be on.
//
// Only the digest is retained across the wait, never the bytes.
func (w rotationWatch) settle(ctx context.Context) {
	if !w.armed {
		return
	}
	started := time.Now()
	interval := rotationSettleFirstPoll
	readErrors := 0
	for {
		if w.baseline {
			switch data, err := w.read(); {
			case err != nil:
				readErrors++
			case sha256.Sum256(data) != w.digest:
				return
			}
		}
		select {
		case <-ctx.Done():
			w.reportUnsettled(time.Since(started), readErrors)
			return
		case <-time.After(interval):
		}
		if interval = time.Duration(float64(interval) * rotationSettlePollGrow); interval > rotationSettleMaxPoll {
			interval = rotationSettleMaxPoll
		}
	}
}

// reportUnsettled is the only record that a rotation may have been spent
// without ever reaching disk. Nothing downstream can tell the difference until
// the next use of the account fails, so this line is loud on purpose.
func (w rotationWatch) reportUnsettled(waited time.Duration, readErrors int) {
	if !w.baseline {
		log.Printf(
			"claude: probe held the CLI %s for an unobservable token rotation; "+
				"if a refresh happened, whether it was saved is unknown",
			waited.Round(time.Millisecond),
		)
		return
	}
	log.Printf(
		"claude: probe waited %s for an expected token rotation and the credential never "+
			"changed (%d unreadable polls); the refresh may have been lost and the account "+
			"may need signing in again",
		waited.Round(time.Millisecond),
		readErrors,
	)
}
