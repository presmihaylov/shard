// echo is the upstream the e2e puts behind the proxy: it answers every request with the headers it saw.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "echo:", err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("address", "", "the host address both listeners bind")
	names := flag.String("names", "", "comma list of the host names the tls certificate is minted for")
	certOut := flag.String("cert-out", "", "where the self-signed certificate is written, for the proxy to trust")
	ready := flag.String("ready", "", "the file written once both listeners are bound")
	flag.Parse()

	if *address == "" || *names == "" || *certOut == "" || *ready == "" {
		return errors.New("-address, -names, -cert-out and -ready are all required")
	}

	cert, certPEM, err := selfSigned(strings.Split(*names, ","))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*certOut, certPEM, 0o600); err != nil {
		return fmt.Errorf("write the certificate: %w", err)
	}

	// A busy port is refused here, before the run counts on the echo, so the failure names the port.
	plain, err := net.Listen("tcp", net.JoinHostPort(*address, "80"))
	if err != nil {
		return fmt.Errorf("listen on %s:80: %w", *address, err)
	}
	secure, err := net.Listen("tcp", net.JoinHostPort(*address, "443"))
	if err != nil {
		return fmt.Errorf("listen on %s:443: %w", *address, err)
	}

	if err := os.WriteFile(*ready, []byte("ready\n"), 0o600); err != nil {
		return fmt.Errorf("write the ready file: %w", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// The e2e reads the headers back as plain text; no browser ever sees this.
		fmt.Fprintf(w, "host=%s\nauthorization=%s\nx-e2e-auth=%s\n", r.Host, r.Header.Get("Authorization"), r.Header.Get("X-E2E-Auth")) //nolint:gosec
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	tlsServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}

	errs := make(chan error, 2)
	go func() { errs <- server.Serve(plain) }()
	go func() { errs <- tlsServer.ServeTLS(secure, "", "") }()

	return <-errs
}

func selfSigned(names []string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate the key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("mint the certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("encode the key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pair the certificate and the key: %w", err)
	}

	return cert, certPEM, nil
}
