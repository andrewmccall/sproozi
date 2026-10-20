// Package pki owns Sproozi's private gateway inspection authority. It creates
// certificate authorities and serving leaves with the operating system's random
// source, keeps private keys in one private directory and publishes only
// certificate-only trust bundles to workers. It is never an upstream trust root.
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"time"
)

// Policy fixes certificate lifetimes. Leaves renew after RenewFraction of their
// lifetime and never outlive their issuer.
type Policy struct {
	CAValidity    time.Duration
	LeafValidity  time.Duration
	RenewFraction float64
	RotateBefore  time.Duration
}

// DefaultPolicy is the proposed local default: a one-year CA, 30-day leaves
// renewed after two thirds of their lifetime, and CA rotation 60 days before
// expiry.
var DefaultPolicy = Policy{CAValidity: 365 * 24 * time.Hour, LeafValidity: 30 * 24 * time.Hour, RenewFraction: 2.0 / 3, RotateBefore: 60 * 24 * time.Hour}

// minimumLeaf rejects a leaf so short that renewal would be continuous.
const minimumLeaf = time.Hour

func (p Policy) validate() error {
	if p.LeafValidity < minimumLeaf || p.CAValidity < 2*p.LeafValidity || p.RenewFraction <= 0 || p.RenewFraction >= 1 || p.RotateBefore < 0 || p.RotateBefore >= p.CAValidity {
		return errors.New("pki: invalid certificate lifetime policy")
	}
	return nil
}

// Names are the serving identities a leaf must cover. A leaf covers every DNS
// name and IP exactly; wildcard names are rejected.
type Names struct {
	DNS []string `json:"dns"`
	IPs []string `json:"ips"`
}

func (n Names) normalized() (Names, error) {
	out := Names{DNS: slices.Clone(n.DNS), IPs: make([]string, 0, len(n.IPs))}
	for _, name := range out.DNS {
		if name == "" || len(name) > 253 || net.ParseIP(name) != nil || bytes.ContainsAny([]byte(name), "*/ \t\r\n\x00") {
			return Names{}, fmt.Errorf("pki: invalid DNS name %q", name)
		}
	}
	for _, value := range n.IPs {
		ip := net.ParseIP(value)
		if ip == nil || ip.IsUnspecified() {
			return Names{}, fmt.Errorf("pki: invalid IP name %q", value)
		}
		out.IPs = append(out.IPs, ip.String())
	}
	if len(out.DNS)+len(out.IPs) == 0 {
		return Names{}, errors.New("pki: at least one serving name required")
	}
	slices.Sort(out.DNS)
	slices.Sort(out.IPs)
	return Names{DNS: slices.Compact(out.DNS), IPs: slices.Compact(out.IPs)}, nil
}

// Covers reports whether the certificate serves every name.
func (n Names) Covers(cert *x509.Certificate) bool {
	for _, name := range n.DNS {
		if cert.VerifyHostname(name) != nil {
			return false
		}
	}
	for _, ip := range n.IPs {
		if cert.VerifyHostname(ip) != nil {
			return false
		}
	}
	return true
}

// Pair is a PEM certificate and its PEM PKCS#8 private key.
type Pair struct {
	Certificate []byte
	Key         []byte
}

// NewCA creates a self-signed inspection authority.
func NewCA(commonName string, now time.Time, validity time.Duration) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	template, err := baseTemplate(commonName, now, now.Add(validity))
	if err != nil {
		return Pair{}, err
	}
	template.IsCA = true
	template.BasicConstraintsValid = true
	template.MaxPathLenZero = true
	template.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	template.SubjectKeyId = keyID(&key.PublicKey)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return Pair{}, err
	}
	return encode(der, key)
}

// IssueLeaf signs a serving certificate for names. Its lifetime is clamped to
// the issuer's remaining lifetime; an issuer too close to expiry is rejected.
func IssueLeaf(issuer Pair, names Names, now time.Time, validity time.Duration) (Pair, error) {
	caCert, caKey, err := Parse(issuer)
	if err != nil {
		return Pair{}, err
	}
	if !caCert.IsCA {
		return Pair{}, errors.New("pki: issuer is not a certificate authority")
	}
	names, err = names.normalized()
	if err != nil {
		return Pair{}, err
	}
	notAfter := now.Add(validity)
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter
	}
	if notAfter.Sub(now) < minimumLeaf || now.Before(caCert.NotBefore) {
		return Pair{}, errors.New("pki: issuer validity cannot cover a new leaf; rotate the CA")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	commonName := "sproozi-gateway"
	if len(names.DNS) > 0 {
		commonName = names.DNS[0]
	}
	template, err := baseTemplate(commonName, now, notAfter)
	if err != nil {
		return Pair{}, err
	}
	template.BasicConstraintsValid = true
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	template.DNSNames = names.DNS
	for _, ip := range names.IPs {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(ip))
	}
	template.AuthorityKeyId = caCert.SubjectKeyId
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return Pair{}, err
	}
	return encode(der, key)
}

// Parse decodes a pair and checks that its key matches its certificate.
func Parse(p Pair) (*x509.Certificate, crypto.Signer, error) {
	cert, err := ParseCertificate(p.Certificate)
	if err != nil {
		return nil, nil, err
	}
	block, rest := pem.Decode(p.Key)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("pki: one PKCS#8 private key required")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("pki: unsupported private key")
	}
	public, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !public.Equal(cert.PublicKey) {
		return nil, nil, errors.New("pki: private key does not match certificate")
	}
	return cert, signer, nil
}

// ParseCertificate decodes exactly one PEM certificate.
func ParseCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("pki: one PEM certificate required")
	}
	return x509.ParseCertificate(block.Bytes)
}

// Fingerprint is the lowercase SHA-256 of the certificate's DER bytes.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// RenewAt is when a leaf with this lifetime should be replaced.
func (p Policy) RenewAt(cert *x509.Certificate) time.Time {
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotBefore.Add(time.Duration(float64(lifetime) * p.RenewFraction))
}

// RotateAt is when routine rotation of this CA should begin.
func (p Policy) RotateAt(ca *x509.Certificate) time.Time {
	return ca.NotAfter.Add(-p.RotateBefore)
}

// VerifyServing checks that leaf chains to one of the trusted roots and serves
// every name at now.
func VerifyServing(leaf *x509.Certificate, trust []byte, names Names, now time.Time) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return errors.New("pki: trust bundle has no certificates")
	}
	if len(names.DNS)+len(names.IPs) == 0 {
		return errors.New("pki: serving names required")
	}
	for _, name := range append(slices.Clone(names.DNS), names.IPs...) {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("pki: serving certificate does not verify for %s: %w", name, err)
		}
	}
	return nil
}

func baseTemplate(commonName string, notBefore, notAfter time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	// Tolerate small clock differences between host and engine without
	// enlarging the advertised lifetime.
	return &x509.Certificate{SerialNumber: serial.Add(serial, big.NewInt(1)), Subject: pkix.Name{CommonName: commonName, Organization: []string{"Sproozi local inspection"}},
		NotBefore: notBefore.Add(-5 * time.Minute).UTC().Truncate(time.Second), NotAfter: notAfter.UTC().Truncate(time.Second)}, nil
}

func keyID(key *ecdsa.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(key)
	sum := sha256.Sum256(der)
	return sum[:20]
}

func encode(der []byte, key *ecdsa.PrivateKey) (Pair, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Pair{}, err
	}
	return Pair{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}, nil
}
