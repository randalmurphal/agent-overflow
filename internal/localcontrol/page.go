package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"

	"agent-overflow/internal/pagehost"
	"agent-overflow/internal/transport"
)

// Page lets a local desktop window adopt a backend the service already owns.
// The credential stays in a header and the returned ticket stays outside the
// navigation URL, exactly as it does when shell and backend share a process.
func Page(ctx context.Context, endpoint Endpoint) (pagehost.Answer, error) {
	var answer pagehost.Answer
	if err := endpoint.validate(); err != nil {
		return answer, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint.Address+transport.PageURLPath+"?"+pagehost.Param+"="+pagehost.Webview, nil)
	if err != nil {
		return answer, errors.New("invalid local page request")
	}
	request.Header.Set("Authorization", "Bearer "+endpoint.Token)
	t := &http.Transport{Proxy: nil}
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("local page redirects are refused") }}
	response, err := client.Do(request)
	if err != nil {
		return answer, errors.New("the local backend is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return answer, errors.New("the local backend did not provide a desktop page")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&answer); err != nil {
		return pagehost.Answer{}, errors.New("invalid local desktop page")
	}
	page, err := url.Parse(answer.URL)
	if err != nil || page.Scheme != "http" || page.User != nil || answer.Ticket == "" {
		return pagehost.Answer{}, errors.New("invalid local desktop page")
	}
	ip := net.ParseIP(page.Hostname())
	if page.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return pagehost.Answer{}, errors.New("the desktop page must stay on this computer")
	}
	return answer, nil
}
