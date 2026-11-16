package git

import (
	"strings"
	"time"
)

const (
	// fetchRetryBase and fetchRetryMax space the unattended fetches of a
	// repository whose last one failed: the base after the first failure,
	// doubling per consecutive failure up to the max. Each is a minimum:
	// the fetch runs on the cadence's first tick past it. A remote that
	// keeps failing (a lapsed login, an outage) is asked less and less
	// often, and a credential helper that opens a window despite gitenv
	// is started at most once per delay.
	fetchRetryBase = time.Minute
	fetchRetryMax  = 15 * time.Minute
)

// fetchFailure is a repository's unattended fetches failing.
type fetchFailure struct {
	// failures counts the consecutive failed fetches retryAt backs off over.
	failures int
	// recordedAt is when the last of them was recorded.
	recordedAt time.Time
	// retryAt is when FetchRemotesBackground fetches again.
	retryAt time.Time
	// err is the last failure, returned again until retryAt.
	err error
	// signIn is that failure's diagnosis when the remote wanted a login the
	// fetch could not give without asking; empty for any other failure.
	signIn string
	// origin is the origin URL the fetch failed against. The record says
	// nothing about any other: fetchBackingOff ends it when origin moves.
	origin string
	// expedited is set once retryFetchNow has made a retry due early, and
	// lasts as long as the fetches keep failing.
	expedited bool
}

// signInDiagnoses are the phrases git, ssh and Git Credential Manager print
// when a remote wanted credentials an unattended command could not supply.
// Lower case; fetchOrigin pins git's messages to the C locale.
var signInDiagnoses = []string{
	"terminal prompts disabled",
	"could not read username",
	"could not read password",
	"authentication failed",
	"permission denied (",
	"host key verification failed",
	"user interactivity has been disabled",
	"returned error: 401",
	"returned error: 403",
}

// fetchSignInDiagnosis is err as a client shows it when an unattended fetch
// failed for want of a login, with remote URLs redacted; empty when it
// failed for another reason.
func fetchSignInDiagnosis(err error) string {
	text := strings.ToLower(err.Error())
	for _, phrase := range signInDiagnoses {
		if strings.Contains(text, phrase) {
			return boundLookupDetail(err.Error())
		}
	}
	return ""
}

func fetchRetryDelay(failures int) time.Duration {
	delay := fetchRetryBase
	for i := 1; i < failures && delay < fetchRetryMax; i++ {
		delay *= 2
	}
	return min(delay, fetchRetryMax)
}

// fetchBackingOff returns the repository's last unattended fetch failure
// while its retry is not due, nil otherwise. origin is the repository's
// origin URL now: a record made against another, or one since removed,
// ends here.
func (c *Core) fetchBackingOff(key, origin string) error {
	c.fetchCacheMu.RLock()
	failure, ok := c.fetchFailures[key]
	c.fetchCacheMu.RUnlock()
	if !ok {
		return nil
	}
	if origin != failure.origin {
		c.forgetFetchFailure(key)
		return nil
	}
	if c.nowFn().Before(failure.retryAt) {
		return failure.err
	}
	return nil
}

// recordFetchFailure records that the unattended fetch of origin a caller
// began waiting for at started failed with err. Anything recorded after
// started already answered for the remote: a fetch that succeeded since
// makes this failure stale, and a failure recorded since is the same fetch
// reported by another of its waiters.
func (c *Core) recordFetchFailure(key, origin string, started time.Time, err error) {
	now := c.nowFn()
	c.fetchCacheMu.Lock()
	previous := c.fetchFailures[key]
	if c.fetchCache[key].After(started) || previous.recordedAt.After(started) {
		c.fetchCacheMu.Unlock()
		return
	}
	failure := fetchFailure{
		failures: previous.failures + 1, recordedAt: now, err: err,
		signIn: fetchSignInDiagnosis(err), origin: origin, expedited: previous.expedited,
	}
	failure.retryAt = now.Add(fetchRetryDelay(failure.failures))
	c.fetchFailures[key] = failure
	// A repository still fetched has retried within fetchRetryMax of its
	// retryAt, so an older record belongs to one this process stopped
	// fetching.
	for k, other := range c.fetchFailures {
		if now.Sub(other.retryAt) > 2*fetchRetryMax {
			delete(c.fetchFailures, k)
		}
	}
	c.fetchCacheMu.Unlock()
	if failure.signIn != previous.signIn {
		c.notifyRemoteSignIn(key)
	}
}

// forgetFetchFailureLocked drops key's failure record and reports whether
// that changed what RemoteSignInError answers. Callers hold fetchCacheMu.
func (c *Core) forgetFetchFailureLocked(key string) bool {
	failure, ok := c.fetchFailures[key]
	delete(c.fetchFailures, key)
	return ok && failure.signIn != ""
}

// retryFetchNow makes a failing repository's next unattended fetch due at
// once, keeping its record until that fetch answers. runSpec calls it after
// a remote command of the repository succeeded: the login may be back, but
// a pull from another remote or a push does not prove origin's fetch is.
// It does so once while the fetches keep failing, so a repository that
// keeps pushing to a working remote does not hold origin's backoff open.
func (c *Core) retryFetchNow(key string) {
	c.fetchCacheMu.Lock()
	defer c.fetchCacheMu.Unlock()
	if failure, ok := c.fetchFailures[key]; ok && !failure.expedited {
		failure.retryAt = c.nowFn()
		failure.expedited = true
		c.fetchFailures[key] = failure
	}
}

// forgetFetchFailure ends key's failure record: the next unattended fetch
// is due at once and the sign-in mark goes.
func (c *Core) forgetFetchFailure(key string) {
	c.fetchCacheMu.Lock()
	signInCleared := c.forgetFetchFailureLocked(key)
	c.fetchCacheMu.Unlock()
	if signInCleared {
		c.notifyRemoteSignIn(key)
	}
}

// ForgetFetchFailures ends every repository's failure record and tells the
// observer of each sign-in mark that went. For the owner of the unattended
// cadence when the cadence is turned off: nothing would retry them.
func (c *Core) ForgetFetchFailures() {
	c.fetchCacheMu.Lock()
	var cleared []string
	for key, failure := range c.fetchFailures {
		if failure.signIn != "" {
			cleared = append(cleared, key)
		}
	}
	clear(c.fetchFailures)
	c.fetchCacheMu.Unlock()
	for _, key := range cleared {
		c.notifyRemoteSignIn(key)
	}
}

// RemoteSignInError is git's diagnosis when the last unattended fetch of
// cwd's repository failed because origin wanted a login; empty when it did
// not, or when cwd is not in a repository.
func (c *Core) RemoteSignInError(cwd string) string {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return ""
	}
	c.fetchCacheMu.RLock()
	defer c.fetchCacheMu.RUnlock()
	return c.fetchFailures[key].signIn
}

// SetRemoteSignInObserver installs fn to be told the common dir of a
// repository whose RemoteSignInError changed, so its owner can re-read the
// statuses that carry it: a failed fetch writes nothing a watcher sees.
// nil removes it. fn runs on the goroutine of the fetch that changed it.
func (c *Core) SetRemoteSignInObserver(fn func(commonDir string)) {
	if fn == nil {
		c.remoteSignInObserver.Store(nil)
		return
	}
	c.remoteSignInObserver.Store(&fn)
}

func (c *Core) notifyRemoteSignIn(commonDir string) {
	if fn := c.remoteSignInObserver.Load(); fn != nil {
		(*fn)(commonDir)
	}
}
