// Package proxy is the intercepting HTTP and TLS proxy a fronted sandbox's web traffic goes through.
package proxy

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

const (
	// CertFile is the CA certificate under the proxy directory, which a fronted sandbox is made to trust.
	CertFile = "ca.crt"
	keyFile  = "ca.key"
	lockFile = "ca.lock"

	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 24 * time.Hour
	// leafCap bounds the leaf cache, so a guest that names a million hosts does not hold a million keys.
	leafCap = 1024

	dirPerm  = 0o700
	keyPerm  = 0o600
	certPerm = 0o644
)

// CA signs a leaf certificate for every host the proxy terminates, from one key minted per root.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	mu     sync.Mutex
	leaves map[string]*list.Element
	order  *list.List
}

type leaf struct {
	host string
	cert *tls.Certificate
}

func leafOf(el *list.Element) *leaf {
	cached, ok := el.Value.(*leaf)
	if !ok {
		panic("the leaf cache holds something that is not a leaf")
	}

	return cached
}

// LoadCA reads the CA under dir and mints one when there is none, under a lock so two callers get one CA.
func LoadCA(dir string) (ca *CA, err error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	lock, err := store.Acquire(filepath.Join(dir, lockFile), keyPerm)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	ca, err = readCA(dir)
	if err == nil {
		return ca, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return mintCA(dir)
}

func readCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		return nil, fmt.Errorf("read the proxy CA: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, fmt.Errorf("read the proxy CA key: %w", err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse the proxy CA under %s: %w", dir, err)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the proxy CA key under %s is not an ECDSA key", dir)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse the proxy CA certificate under %s: %w", dir, err)
	}

	return newCA(cert, key, certPEM), nil
}

func mintCA(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the proxy CA key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shard egress proxy CA", Organization: []string{"shard"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("sign the proxy CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the proxy CA: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the proxy CA key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// The key lands first, so a crash between the two leaves a key nothing trusts and never a cert nothing can sign with.
	if err := store.WriteFile(filepath.Join(dir, keyFile), keyPEM, keyPerm); err != nil {
		return nil, fmt.Errorf("write the proxy CA key: %w", err)
	}
	if err := store.WriteFile(filepath.Join(dir, CertFile), certPEM, certPerm); err != nil {
		return nil, fmt.Errorf("write the proxy CA: %w", err)
	}

	return newCA(cert, key, certPEM), nil
}

func newCA(cert *x509.Certificate, key *ecdsa.PrivateKey, certPEM []byte) *CA {
	return &CA{cert: cert, key: key, pem: certPEM, leaves: map[string]*list.Element{}, order: list.New()}
}

// CertPEM is the certificate a guest must trust, and never the key.
func (ca *CA) CertPEM() []byte { return append([]byte(nil), ca.pem...) }

// Leaf answers the certificate for host, minted on the first ask and kept in a bounded cache.
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	if el, ok := ca.leaves[host]; ok {
		cached := leafOf(el)
		if time.Now().Before(cached.cert.Leaf.NotAfter.Add(-time.Hour)) {
			ca.order.MoveToFront(el)

			return cached.cert, nil
		}
		ca.order.Remove(el)
		delete(ca.leaves, host)
	}

	cert, err := ca.mint(host)
	if err != nil {
		return nil, err
	}

	ca.leaves[host] = ca.order.PushFront(&leaf{host: host, cert: cert})
	for ca.order.Len() > leafCap {
		oldest := ca.order.Back()
		delete(ca.leaves, leafOf(oldest).host)
		ca.order.Remove(oldest)
	}

	return cert, nil
}

func (ca *CA) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key for %s: %w", host, err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	}
	if template.IPAddresses == nil {
		template.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign a certificate for %s: %w", host, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse the certificate for %s: %w", host, err)
	}

	return &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: parsed}, nil
}

func serialNumber() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("draw a serial number: %w", err)
	}

	return serial, nil
}
