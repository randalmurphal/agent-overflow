package localcontrol

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"agent-overflow/internal/atomicfile"
)

const Filename = "control.json"

// Endpoint is private launch state, never a public diagnostic payload.
type Endpoint struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

func (e Endpoint) validate() error {
	host, port, err := net.SplitHostPort(e.Address)
	if err != nil {
		return errors.New("invalid local control address")
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || n < 1 || n > 65535 || e.Token == "" || len(e.Token) > 1024 {
		return errors.New("invalid local control endpoint")
	}
	return nil
}
func Publish(root, address, token string) error {
	if root == "" {
		return errors.New("local control requires an app data directory")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid backend listener address")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	endpoint := Endpoint{Address: net.JoinHostPort(host, port), Token: token}
	if err := endpoint.validate(); err != nil {
		return err
	}
	return atomicfile.WriteJSON(filepath.Join(root, Filename), endpoint)
}
func Read(root string) (Endpoint, error) {
	var endpoint Endpoint
	if root == "" {
		return endpoint, errors.New("local control requires an app data directory")
	}
	file, err := os.Open(filepath.Join(root, Filename))
	if err != nil {
		return endpoint, errors.New("no running backend was found; start agent-overflow serve or agent-overflow service start")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	if err := decoder.Decode(&endpoint); err != nil {
		return Endpoint{}, errors.New("invalid local control file")
	}
	if err := endpoint.validate(); err != nil {
		return Endpoint{}, err
	}
	return endpoint, nil
}
func Withdraw(root, token string) error {
	endpoint, err := Read(root)
	if err != nil || endpoint.Token != token {
		return nil
	}
	return os.Remove(filepath.Join(root, Filename))
}
