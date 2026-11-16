package forgeapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writePEM writes blocks of kind to a file in dir and returns its path.
func writePEM(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clientCertificate is a self-signed client certificate and its key,
// written as PEM files in dir, and the pool that trusts it.
func clientCertificate(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "glab client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return writePEM(t, dir, "client.crt", "CERTIFICATE", der), writePEM(t, dir, "client.key", "PRIVATE KEY", keyDER), pool
}

// A GitLab host's glab TLS and proxy settings reach its requests: the
// host's private CA is trusted (and only with ca_cert), skip_tls_verify
// skips verification, client_cert and client_key answer a server that
// requires a client certificate, and proxy carries the requests. A
// setting that cannot be used is the login's setup error, sent nowhere.
func TestGitLabHostNetworkSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	answer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	private := httptest.NewTLSServer(answer)
	defer private.Close()
	caPath := writePEM(t, dir, "ca.crt", "CERTIFICATE", private.Certificate().Raw)
	read := func(srv *httptest.Server, n HostNetwork) error {
		t.Helper()
		s, _ := liveService(t, &testSource{tokens: []string{testSecret}, info: SourceInfo{Header: AuthPrivateToken, Network: n}}, srv)
		_, err := s.GitLab("gl.example").Do(t.Context(), Request{Path: "user"})
		return err
	}
	if err := read(private, HostNetwork{}); err == nil {
		t.Fatal("a private CA was trusted without ca_cert")
	}
	if err := read(private, HostNetwork{CACert: caPath}); err != nil {
		t.Fatalf("with ca_cert: %v", err)
	}
	if err := read(private, HostNetwork{SkipTLSVerify: true}); err != nil {
		t.Fatalf("with skip_tls_verify: %v", err)
	}

	certPath, keyPath, clients := clientCertificate(t, dir)
	mutual := httptest.NewUnstartedServer(answer)
	mutual.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clients}
	mutual.StartTLS()
	defer mutual.Close()
	mutualCA := writePEM(t, dir, "mutual-ca.crt", "CERTIFICATE", mutual.Certificate().Raw)
	if err := read(mutual, HostNetwork{CACert: mutualCA}); err == nil {
		t.Fatal("a server requiring a client certificate answered without one")
	}
	if err := read(mutual, HostNetwork{CACert: mutualCA, ClientCert: certPath, ClientKey: keyPath}); err != nil {
		t.Fatalf("with client_cert and client_key: %v", err)
	}

	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "" {
			proxied.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the request went around the proxy")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	if err := read(target, HostNetwork{Proxy: proxy.URL}); err != nil || proxied.Load() != 1 {
		t.Fatalf("with proxy: %v, %d proxied", err, proxied.Load())
	}

	for _, bad := range []HostNetwork{
		{CACert: filepath.Join(dir, "missing.crt")},
		{CACert: keyPath},
		{ClientCert: certPath},
		{Proxy: "not a url"},
	} {
		err := read(private, bad)
		if setup, ok := errors.AsType[*SetupError](err); !ok || setup.Kind != SetupMissing || !strings.Contains(setup.Message, "glab's settings for gl.example cannot be used") {
			t.Fatalf("%+v = %v, want the setup error", bad, err)
		}
	}
}
