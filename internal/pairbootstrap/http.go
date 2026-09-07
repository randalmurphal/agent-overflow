package pairbootstrap

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const Path = "/auth/pair/nearby"

var ErrAddress = errors.New("Enter a hostname or IP address, with a port if needed")

// NewHTTPClient is exclusively for the credential-free, committed bootstrap.
// The invitation's authenticated pin is enforced by the ordinary pairing
// client afterward. Never use this transport for a session, token or invite.
func NewHTTPClient(dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second},
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func NormalizeAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 1024 {
		return "", ErrAddress
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrAddress
	}
	if strings.ContainsAny(u.Hostname(), " \\%\t\r\n") || strings.HasSuffix(u.Host, ":") {
		return "", ErrAddress
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrAddress
		}
	}
	u.Path = ""
	return u.String(), nil
}

type Request struct {
	Op         string     `json:"op"`
	Commitment Commitment `json:"commitment,omitempty"`
	ID         string     `json:"id,omitempty"`
	Reveal     Reveal     `json:"reveal,omitempty"`
}

// Exchange uses only a fresh, credential-free HTTP transport. Its initial TLS
// peer may be untrusted: trust comes from the independently computed comparison
// on the host and client screens, not from discovery or these HTTP responses.
// No redirect, cookie jar, authentication header or implicit mutation retry is
// allowed. The returned invitation is redeemed by the ordinary pinned client.
func Exchange(ctx context.Context, transport *http.Client, endpoint, label, platform string) (Invitation, string, error) {
	endpoint, err := NormalizeAddress(endpoint)
	if err != nil {
		return Invitation{}, "", err
	}
	if transport == nil {
		transport = NewHTTPClient(nil)
	}
	hc := *transport
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := Start(label, platform)
	if err != nil {
		return Invitation{}, "", err
	}
	endpoint = strings.TrimRight(endpoint, "/") + Path
	var ch Challenge
	if err = exchange(ctx, &hc, endpoint, Request{Op: "begin", Commitment: client.Commitment()}, &ch); err != nil {
		return Invitation{}, "", err
	}
	r, err := client.Reveal(ch)
	if err != nil {
		return Invitation{}, "", err
	}
	var sealed SealedInvitation
	if err = exchange(ctx, &hc, endpoint, Request{Op: "reveal", ID: ch.ID, Reveal: r}, &sealed); err != nil {
		return Invitation{}, "", err
	}
	return client.Open(sealed)
}

func exchange(ctx context.Context, hc *http.Client, endpoint string, body Request, result any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Pairing request failed (HTTP %d); open Pair a device on the other computer and try again", resp.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil {
		return err
	}
	if len(b) > 16384 || json.Unmarshal(b, result) != nil {
		return ErrInvalid
	}
	return nil
}
