package linear

import (
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// fake is a lineartest.Server whose clients the tests build here, since
// lineartest cannot import this package.
type fake struct {
	*lineartest.Server
}

func newFake(t *testing.T, script ...lineartest.Exchange) *fake {
	t.Helper()
	return &fake{lineartest.NewServer(t, script...)}
}

// client is a Client of the fake, closed when the test ends.
func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL:     f.URL(),
		Version:     "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(lineartest.Key)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
