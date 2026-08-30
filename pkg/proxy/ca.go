// Package proxy is an intercepting HTTP and TLS proxy. It terminates what a sandbox sends, asks a
// Director where it may go and what to change, and carries it there. It holds no policy of its own.
package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

const (
	caDirPerm  = 0o700
	caKeyPerm  = 0o600
	caCertPerm = 0o644
	// A leaf lives a day and is minted again: a host's name changes nothing, and a short life bounds a stolen one.
	leafLife = 24 * time.Hour
	caLife   = 10 * 365 * 24 * time.Hour
)

// CA signs a certificate for every host a sandbox asks for. A guest that trusts it trusts what the
// proxy says the host is, which is the whole mechanism.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreate reads the CA from dir, or mints one there. The key never leaves the directory.
func LoadOrCreate(dir string) (*CA, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the CA directory must be an absolute path, got %q", dir)
	}

	if err := os.MkdirAll(dir, caDirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	ca, err := load(dir)
	if err == nil {
		return ca, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	return create(dir)
}

func load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("read the CA certificate: %w", err)
	}

	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, fmt.Errorf("read the CA key: %w", err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse the CA in %s: %w", dir, err)
	}

	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the CA key in %s is not an ECDSA key", dir)
	}

	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate in %s: %w", dir, err)
	}

	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

func create(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("pick a serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shard egress proxy CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLife),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("sign the CA certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the CA key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// The key goes first: a certificate with no key is a CA nobody can use, the reverse is a key nobody trusts yet.
	if err := store.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, caKeyPerm); err != nil {
		return nil, fmt.Errorf("write the CA key: %w", err)
	}
	if err := store.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, caCertPerm); err != nil { // #nosec G306
		return nil, fmt.Errorf("write the CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}

	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// PEM is the certificate a guest must trust. It carries no key.
func (ca *CA) PEM() []byte { return ca.pem }

// Leaf is a certificate for host, signed by the CA, minted on the first ask and kept for a day.
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if leaf, ok := ca.leaves[host]; ok && time.Now().Before(leaf.Leaf.NotAfter.Add(-time.Hour)) {
		return leaf, nil
	}

	leaf, err := ca.mint(host)
	if err != nil {
		return nil, err
	}
	ca.leaves[host] = leaf

	return leaf, nil
}

func (ca *CA) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key for %s: %w", host, err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("pick a serial for %s: %w", host, err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign a certificate for %s: %w", host, err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the certificate for %s: %w", host, err)
	}

	return &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: cert}, nil
}
