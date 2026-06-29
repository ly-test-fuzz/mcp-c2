// Package certgen generates self-signed mTLS certificates for MCP-C2.
// On first startup, the hub auto-generates a CA, server cert, and client cert
// if they are not explicitly provided via flags.
package certgen

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	CAValidity       = 10 * 365 * 24 * time.Hour // 10 years
	ServerValidity   = 3 * 365 * 24 * time.Hour  // 3 years
	ClientValidity   = 3 * 365 * 24 * time.Hour  // 3 years
	DefaultCAName    = "MCP-C2 CA"
	DefaultOrg       = "MCP-C2"
)

// Bundle holds a complete set of generated credentials.
type Bundle struct {
	CA   *CertPair
	Server *CertPair
	Client *CertPair
}

// CertPair holds a certificate and its private key (both PEM-encoded).
type CertPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GenerateBundle creates a fresh CA, server cert, and client cert.
// serverIPs / serverDNS are added as SANs on the server certificate.
func GenerateBundle(serverIPs []net.IP, serverDNS []string) (*Bundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	caCert, caPEM, err := selfSignedCA(caKey)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})

	// Server cert
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}
	srvCertPEM, err := issueCert(caCert, caKey, srvKey, serverIPs, serverDNS, ServerValidity, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(srvKey)})

	// Client cert
	cliKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("client key: %w", err)
	}
	cliCertPEM, err := issueCert(caCert, caKey, cliKey, nil, nil, ClientValidity, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, fmt.Errorf("client cert: %w", err)
	}
	cliKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(cliKey)})

	return &Bundle{
		CA:     &CertPair{CertPEM: caPEM, KeyPEM: caKeyPEM},
		Server: &CertPair{CertPEM: srvCertPEM, KeyPEM: srvKeyPEM},
		Client: &CertPair{CertPEM: cliCertPEM, KeyPEM: cliKeyPEM},
	}, nil
}

// Save writes the bundle to disk.
func (b *Bundle) Save(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	write := func(name string, data []byte) error {
		return os.WriteFile(filepath.Join(dir, name), data, 0600)
	}
	if err := write("ca.crt", b.CA.CertPEM); err != nil {
		return err
	}
	if err := write("ca.key", b.CA.KeyPEM); err != nil {
		return err
	}
	if err := write("server.crt", b.Server.CertPEM); err != nil {
		return err
	}
	if err := write("server.key", b.Server.KeyPEM); err != nil {
		return err
	}
	if err := write("client.crt", b.Client.CertPEM); err != nil {
		return err
	}
	if err := write("client.key", b.Client.KeyPEM); err != nil {
		return err
	}
	return nil
}

// ClientFingerprint returns the SHA-256 fingerprint of the client certificate.
func (b *Bundle) ClientFingerprint() string {
	block, _ := pem.Decode(b.Client.CertPEM)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// ── internal helpers ────────────────────────────────────────────────────

func selfSignedCA(key *rsa.PrivateKey) (*x509.Certificate, []byte, error) {
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: DefaultCAName, Organization: []string{DefaultOrg}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, pemBytes, nil
}

func issueCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, pubKey *rsa.PrivateKey, ips []net.IP, dns []string, validity time.Duration, extKeyUsage x509.ExtKeyUsage) ([]byte, error) {
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: DefaultCAName + " Leaf", Organization: []string{DefaultOrg}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{extKeyUsage},
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &pubKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func randomSerial() *big.Int {
	s, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return s
}
