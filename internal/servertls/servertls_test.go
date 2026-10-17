package servertls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/kube-distribution-network/internal/config"
)

func TestDisabledReturnsNil(t *testing.T) {
	tc, err := New(config.TLS{})
	if err != nil || tc != nil {
		t.Fatalf("New(empty) = %v, %v; want nil, nil", tc, err)
	}
}

func TestBadFilesFailAtStartup(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "server-ca")
	certFile, keyFile := ca.issue(t, dir, "server", "localhost", x509.ExtKeyUsageServerAuth)
	other := newCA(t, "other")
	_, otherKey := other.issue(t, dir, "other", "localhost", x509.ExtKeyUsageServerAuth)
	notPEM := writeFile(t, dir, "junk.pem", []byte("not a certificate"))

	cases := map[string]config.TLS{
		"missing cert":      {CertFile: filepath.Join(dir, "nope.crt"), KeyFile: keyFile},
		"mismatched key":    {CertFile: certFile, KeyFile: otherKey},
		"missing client CA": {CertFile: certFile, KeyFile: keyFile, ClientCAFile: filepath.Join(dir, "nope.pem")},
		"client CA not PEM": {CertFile: certFile, KeyFile: keyFile, ClientCAFile: notPEM},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTLS(t *testing.T) {
	dir := t.TempDir()
	serverCA := newCA(t, "server-ca")
	certFile, keyFile := serverCA.issue(t, dir, "server", "127.0.0.1", x509.ExtKeyUsageServerAuth)

	srv := serve(t, config.TLS{CertFile: certFile, KeyFile: keyFile})

	// Without mTLS, a client certificate is not required.
	if err := get(srv, serverCA.pool(), nil); err != nil {
		t.Fatalf("TLS request failed: %v", err)
	}
}

func TestMutualTLS(t *testing.T) {
	dir := t.TempDir()
	serverCA := newCA(t, "server-ca")
	certFile, keyFile := serverCA.issue(t, dir, "server", "127.0.0.1", x509.ExtKeyUsageServerAuth)

	cdnCA := newCA(t, "cdn-ca")
	clientCAFile := writeFile(t, dir, "client-ca.pem", cdnCA.certPEM)
	goodCert, goodKey := cdnCA.issue(t, dir, "cdn", "cdn", x509.ExtKeyUsageClientAuth)

	impostorCA := newCA(t, "impostor-ca")
	badCert, badKey := impostorCA.issue(t, dir, "impostor", "cdn", x509.ExtKeyUsageClientAuth)

	srv := serve(t, config.TLS{CertFile: certFile, KeyFile: keyFile, ClientCAFile: clientCAFile})

	if err := get(srv, serverCA.pool(), nil); err == nil {
		t.Error("a client with no certificate was accepted")
	}
	if err := get(srv, serverCA.pool(), loadPair(t, badCert, badKey)); err == nil {
		t.Error("a client certificate from an untrusted CA was accepted")
	}
	if err := get(srv, serverCA.pool(), loadPair(t, goodCert, goodKey)); err != nil {
		t.Errorf("a client certificate from the trusted CA was refused: %v", err)
	}
}

func TestCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	oldCA := newCA(t, "old-ca")
	certFile, keyFile := oldCA.issue(t, dir, "server", "127.0.0.1", x509.ExtKeyUsageServerAuth)

	r := &reloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		t.Fatal(err)
	}
	before, _ := r.getCertificate(nil)

	// Rotate in place, as cert-manager or a Secret update would.
	newCA := newCA(t, "new-ca")
	newCA.issue(t, dir, "server", "127.0.0.1", x509.ExtKeyUsageServerAuth)
	future := time.Now().Add(time.Minute)
	for _, f := range []string{certFile, keyFile} {
		if err := os.Chtimes(f, future, future); err != nil {
			t.Fatal(err)
		}
	}

	// Within the interval, the cached certificate is still served.
	if got, _ := r.getCertificate(nil); got != before {
		t.Error("certificate was reloaded before the reload interval elapsed")
	}

	r.checked = time.Now().Add(-2 * reloadInterval)
	after, _ := r.getCertificate(nil)
	if after == before {
		t.Fatal("rotated certificate was not picked up")
	}
	if after.Leaf.Issuer.CommonName != "new-ca" {
		t.Errorf("issuer = %q, want new-ca", after.Leaf.Issuer.CommonName)
	}

	// A broken rotation keeps the last good certificate.
	writeFile(t, dir, "server.key", []byte("garbage"))
	future = future.Add(time.Minute)
	_ = os.Chtimes(keyFile, future, future)
	r.checked = time.Now().Add(-2 * reloadInterval)
	if got, _ := r.getCertificate(nil); got != after {
		t.Error("a failed reload replaced a working certificate")
	}
}

// --- helpers ---

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// issue writes a leaf certificate and key to dir/<name>.crt and dir/<name>.key.
func (ca *testCA) issue(t *testing.T, dir, name, host string, usage x509.ExtKeyUsage) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = writeFile(t, dir, name+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyFile = writeFile(t, dir, name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadPair(t *testing.T, certFile, keyFile string) *tls.Certificate {
	t.Helper()
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

// serve starts a TLS server with the config New builds and returns its URL.
// httptest.Server is not used because it installs its own certificate, which
// would take precedence over GetCertificate for a client sending no SNI.
func serve(t *testing.T, cfg config.TLS) string {
	t.Helper()
	tc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = srv.Serve(tls.NewListener(ln, tc)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func get(url string, roots *x509.CertPool, clientCert *tls.Certificate) error {
	tc := &tls.Config{RootCAs: roots}
	if clientCert != nil {
		tc.Certificates = []tls.Certificate{*clientCert}
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
