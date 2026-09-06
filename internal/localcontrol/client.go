package localcontrol

import (
	"context"
	"errors"
	"net/http"

	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
)

// Client serializes small owner RPCs without retaining event history.
type Client = rpcclient.Client

func Dial(ctx context.Context, endpoint Endpoint) (*Client, error) {
	if err := endpoint.validate(); err != nil {
		return nil, err
	}
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+endpoint.Token)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local control redirects are refused") }}
	conn, _, err := websocket.Dial(ctx, "ws://"+endpoint.Address+transport.WSPath, &websocket.DialOptions{HTTPHeader: header, HTTPClient: client})
	if err != nil {
		return nil, errors.New("the backend did not accept the local connection; check agent-overflow service status")
	}
	return rpcclient.New(conn), nil
}
