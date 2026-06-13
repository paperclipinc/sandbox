// Package pki provides the internal certificate authority for the
// control plane: the controller and forkd authenticate each other with
// mTLS using exactly two leaf identities issued by this CA.
package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// The only two identities this PKI will ever issue. Restricting
// issuance is a defense against identity sprawl.
const (
	// ServerName is the DNS SAN of the forkd gRPC server certificate.
	ServerName = "forkd.mitos"
	// ControllerName is the DNS SAN of the controller client certificate.
	ControllerName = "controller.mitos"
)

// CA is a self-signed certificate authority for the control plane.
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
	keyPEM  []byte
}

// Leaf is an issued end-entity certificate with its private key,
// both PEM encoded.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a 10-year ECDSA P-256 self-signed CA.
func NewCA(org string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   org + " control plane CA",
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	keyPEM, err := encodeECKeyPEM(key)
	if err != nil {
		return nil, fmt.Errorf("encode CA key: %w", err)
	}

	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  keyPEM,
	}, nil
}

// LoadCA reconstructs a CA from PEM-encoded certificate and key.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("load CA: no CERTIFICATE block in cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("load CA: parse certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("load CA: certificate is not a CA")
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("load CA: no PEM block in key")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("load CA: parse key: %w", err)
	}
	canonicalKeyPEM, err := encodeECKeyPEM(key)
	if err != nil {
		return nil, fmt.Errorf("load CA: encode key: %w", err)
	}

	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}),
		keyPEM:  canonicalKeyPEM,
	}, nil
}

// CertPEM returns the PEM-encoded CA certificate.
func (ca *CA) CertPEM() []byte {
	return ca.certPEM
}

// KeyPEM returns the PEM-encoded CA private key for persistence. The
// encoding happens eagerly in NewCA/LoadCA so a marshal failure
// surfaces there as an error instead of a silent nil here.
func (ca *CA) KeyPEM() []byte {
	return ca.keyPEM
}

func encodeECKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal EC key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// Issue creates a 2-year ECDSA leaf certificate with the given DNS SAN.
// Only the two known control plane identities are accepted, and each
// gets exactly the extended key usage its role needs: the forkd server
// leaf is ServerAuth only, the controller leaf is ClientAuth only. A
// stolen server certificate therefore cannot even complete a client
// handshake; the interceptor's SAN check stays as the second layer.
func (ca *CA) Issue(dnsName string) (*Leaf, error) {
	var eku x509.ExtKeyUsage
	switch dnsName {
	case ServerName:
		eku = x509.ExtKeyUsageServerAuth
	case ControllerName:
		eku = x509.ExtKeyUsageClientAuth
	default:
		return nil, fmt.Errorf("issue %q: only %q and %q may be issued", dnsName, ServerName, ControllerName)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}

	return &Leaf{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// ServerTLSConfig builds the forkd server side of the mTLS pair:
// TLS 1.3 only, client certificates required and verified against caPEM.
func ServerTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("server TLS keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLSConfig builds the controller client side of the mTLS pair:
// TLS 1.3 only, server identity pinned to ServerName, roots from caPEM.
func ClientTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("client TLS keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   ServerName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// PeerDNSName extracts the TLS peer's first DNS SAN from gRPC peer
// info. It only ever reports VERIFIED identities: the SAN comes from
// VerifiedChains, never from the raw PeerCertificates the peer merely
// presented. No verified chain means no identity, even if the peer
// sent a certificate.
func PeerDNSName(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", false
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}

	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	if len(leaf.DNSNames) == 0 {
		return "", false
	}
	return leaf.DNSNames[0], true
}

func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA pool: no valid certificates in PEM")
	}
	return pool, nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}
