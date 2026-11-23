package linear

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

const otherKey = "lin_api_fake1111111111"

// keyFile is a credential source whose key a test replaces, as a key file
// written again would be: read n returns keys[n], the last key once they
// run out. reads counts its reads.
type keyFile struct {
	mu    sync.Mutex
	keys  []string
	reads int
}

func newKeyFile(keys ...string) *keyFile { return &keyFile{keys: keys} }

// set makes key the key every later read returns.
func (k *keyFile) set(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = []string{key}
}

func (k *keyFile) Credential(context.Context) (*trackerapi.Credential, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := k.keys[min(k.reads, len(k.keys)-1)]
	k.reads++
	return trackerapi.Key(secret.New([]byte(key))), nil
}

// boundClient is a client of f for workspace, reading src.
func boundClient(t *testing.T, f *fake, workspace string, src trackerapi.CredentialSource) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: f.URL(), Version: "test", Credentials: src, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// check expects the workspace check with key, answered for urlKey.
func check(key, urlKey string) lineartest.Exchange {
	return lineartest.Exchange{Op: "Viewer", Key: key, Body: lineartest.ViewerAnswer(urlKey)}
}

// teams expects one read of the teams with key.
func teams(key string) lineartest.Exchange {
	return lineartest.Exchange{Op: "Teams", Key: key, Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))}
}

// wrongWorkspace fails the test unless err is a call refused before
// sending because the key is otherWorkspace's.
func wrongWorkspace(t *testing.T, err error, got string) {
	t.Helper()
	ww, ok := errors.AsType[*WrongWorkspaceError](err)
	if _, notSent := errors.AsType[*trackerapi.NotSentError](err); !ok || !notSent || ww.Want != "example" || ww.Got != got {
		t.Errorf("error %v, want a call not sent for a key of workspace %q", err, got)
	}
}

// The right workspace, its URL key in another case, is checked once:
// concurrent first calls share the check, and later calls send none.
func TestBoundWorkspaceChecksOnce(t *testing.T) {
	t.Parallel()
	const calls = 8
	script := []lineartest.Exchange{check(lineartest.Key, "example")}
	for range calls + 1 {
		script = append(script, teams(lineartest.Key))
	}
	f := newFake(t, script...)
	src := newKeyFile(lineartest.Key)
	c := boundClient(t, f, "Example", src)
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			if _, err := c.Teams(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads != 1 {
		t.Errorf("the key was read %d times", src.reads)
	}
}

// A key of another workspace sends nothing but the check: a read and a
// write each fail before sending, naming both workspaces, and no later
// call checks again.
func TestBoundWorkspaceRefusesAnotherWorkspace(t *testing.T) {
	t.Parallel()
	f := newFake(t, check(lineartest.Key, "other"))
	c := boundClient(t, f, "example", newKeyFile(lineartest.Key))
	_, err := c.Teams(context.Background())
	wrongWorkspace(t, err, "other")
	err = c.DeleteComment(context.Background(), "comment-1")
	wrongWorkspace(t, err, "other")
	if !WriteNotApplied(err) {
		t.Errorf("a write never sent may have been applied: %v", err)
	}
	_, err = c.Teams(context.Background())
	wrongWorkspace(t, err, "other")
}

// A key replaced after a good one, read after ForgetCredential, is
// checked before its first call and refused when it is another
// workspace's; ForgetCredential again checks the next key.
func TestBoundWorkspaceChecksAReplacedKey(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		check(lineartest.Key, "example"), teams(lineartest.Key),
		check(otherKey, "other"),
		check(lineartest.Key, "example"), teams(lineartest.Key),
	)
	src := newKeyFile(lineartest.Key)
	c := boundClient(t, f, "example", src)
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.set(otherKey)
	c.ForgetCredential()
	_, err := c.Teams(context.Background())
	wrongWorkspace(t, err, "other")
	src.set(lineartest.Key)
	c.ForgetCredential()
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ForgetCredential during a check that finds another workspace keeps that
// check from refusing the key read after it.
func TestBoundWorkspaceForgetDuringCheck(t *testing.T) {
	t.Parallel()
	var c *Client
	src := newKeyFile(otherKey)
	wrong := check(otherKey, "other")
	wrong.BeforeBody = func() {
		src.set(lineartest.Key)
		c.ForgetCredential()
	}
	f := newFake(t, wrong, check(lineartest.Key, "example"), teams(lineartest.Key))
	c = boundClient(t, f, "example", src)
	_, err := c.Teams(context.Background())
	wrongWorkspace(t, err, "other")
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatalf("the key read after ForgetCredential: %v", err)
	}
}

// ForgetCredential during a check that finds the workspace's keeps the
// checked key from being sent: the transport drops what that read stored
// and reads again, and the key read then is checked before the call is
// sent with it.
func TestBoundWorkspaceForgetDuringAGoodCheck(t *testing.T) {
	t.Parallel()
	var c *Client
	src := newKeyFile(lineartest.Key)
	good := check(lineartest.Key, "example")
	good.BeforeBody = func() {
		src.set(otherKey)
		c.ForgetCredential()
	}
	f := newFake(t, good, check(otherKey, "other"))
	c = boundClient(t, f, "example", src)
	_, err := c.Teams(context.Background())
	wrongWorkspace(t, err, "other")
}

// A 401 reads the key again, and the new key is checked before the call
// is sent again with it: sent when it is the workspace's, never sent
// when it is another's.
func TestBoundWorkspaceChecksAfterRefusal(t *testing.T) {
	t.Parallel()
	refused := func(key string) lineartest.Exchange {
		x := teams(key)
		x.Status, x.Body = http.StatusUnauthorized, authBody
		return x
	}
	const thirdKey = "lin_api_fake2222222222"
	f := newFake(t,
		check(lineartest.Key, "example"), refused(lineartest.Key),
		check(otherKey, "example"), teams(otherKey),
		refused(otherKey), check(thirdKey, "other"),
	)
	src := newKeyFile(lineartest.Key, otherKey)
	c := boundClient(t, f, "example", src)
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatalf("after a 401 and a good key: %v", err)
	}
	src.set(thirdKey)
	_, err := c.Teams(context.Background())
	if _, ok := errors.AsType[*WrongWorkspaceError](err); !ok {
		t.Errorf("after a 401 and another workspace's key: %v", err)
	}
	if _, ok := errors.AsType[*trackerapi.AuthError](err); !ok {
		t.Errorf("the 401 is not reachable: %v", err)
	}
}

// A check that fails for another reason fails the call with its error and
// is tried again on the next call.
func TestBoundWorkspaceRetriesAFailedCheck(t *testing.T) {
	t.Parallel()
	failed := check(lineartest.Key, "example")
	failed.Status, failed.Body = http.StatusInternalServerError, `{"errors":[{"message":"boom"}]}`
	f := newFake(t, failed, check(lineartest.Key, "example"), teams(lineartest.Key))
	c := boundClient(t, f, "example", newKeyFile(lineartest.Key))
	_, err := c.Teams(context.Background())
	if e, ok := errors.AsType[*Error](err); !ok || e.Status != http.StatusInternalServerError {
		t.Errorf("the check's failure is not the call's: %v", err)
	}
	if _, ok := errors.AsType[*WrongWorkspaceError](err); ok {
		t.Errorf("a failed check reads as another workspace: %v", err)
	}
	if _, err := c.Teams(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Viewer on a bound client compares its own answer; an unbound client
// sends no check.
func TestBoundWorkspaceViewer(t *testing.T) {
	t.Parallel()
	f := newFake(t,
		check(lineartest.Key, "example"),
		lineartest.Exchange{Op: "Viewer", Body: lineartest.ViewerAnswer("other")},
		teams(lineartest.Key),
	)
	_, err := boundClient(t, f, "example", newKeyFile(lineartest.Key)).Viewer(context.Background())
	if ww, ok := errors.AsType[*WrongWorkspaceError](err); !ok || ww.Got != "other" {
		t.Errorf("Viewer's own answer of another workspace: %v", err)
	}
	if _, err := f.client(t).Teams(context.Background()); err != nil {
		t.Errorf("unbound: %v", err)
	}
}
