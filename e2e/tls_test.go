//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

var (
	tlsPort string
	tlsCA   []byte
)

// startTLSFront terminates TLS for clients that refuse plain-HTTP sources and
// forwards to the server with X-Forwarded-Proto set, as a real ingress would.
func startTLSFront(backend string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "artifactapi-e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{testcontainers.HostInternal, "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	tlsCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	target, _ := url.Parse(backend)
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme = target.Scheme
		pr.Out.URL.Host = target.Host
		pr.Out.Host = pr.In.Host
		pr.SetXForwarded()
	}}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	_, tlsPort, _ = net.SplitHostPort(ln.Addr().String())
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	go func() { _ = http.Serve(tlsLn, proxy) }()
	return nil
}
