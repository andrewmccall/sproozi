package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// certificateReloader replaces the serving certificate when the mounted
// Secret files change. Kubernetes updates projected Secret files in place, so
// a renewed leaf reaches new connections without restarting the gateway.
type certificateReloader struct {
	certFile, keyFile string
	hosts             []string
	holder            *proxytransport.Certificate
	last              []byte
}

const certificateReloadInterval = 30 * time.Second

// reload installs the files' certificate when it changed and still covers
// every inspected host. A rejected replacement keeps the current certificate.
func (r *certificateReloader) reload() (bool, error) {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return false, err
	}
	current := append(slices.Clone(certPEM), keyPEM...)
	if bytes.Equal(current, r.last) {
		return false, nil
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, err
	}
	if certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0]); err != nil {
		return false, err
	}
	if err = r.verifyChain(certificate.Leaf); err != nil {
		return false, err
	}
	if time.Now().After(certificate.Leaf.NotAfter) {
		return false, fmt.Errorf("replacement certificate expired at %s", certificate.Leaf.NotAfter)
	}
	r.holder.Store(certificate)
	r.last = current
	return true, nil
}

// verifyChain checks every inspected host. When the Secret also carries the
// published trust as ca.crt, as the certificate controller writes it, the leaf
// must chain to that trust; otherwise only the names are checked.
func (r *certificateReloader) verifyChain(leaf *x509.Certificate) error {
	trust, err := os.ReadFile(filepath.Join(filepath.Dir(r.certFile), "ca.crt"))
	if os.IsNotExist(err) {
		for _, host := range r.hosts {
			if err = leaf.VerifyHostname(host); err != nil {
				return fmt.Errorf("replacement certificate does not cover %q", host)
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return errors.New("replacement trust bundle has no certificates")
	}
	for _, host := range r.hosts {
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			return fmt.Errorf("replacement certificate does not verify for %q: %w", host, err)
		}
	}
	return nil
}

func (r *certificateReloader) run(ctx context.Context) {
	ticker := time.NewTicker(certificateReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := r.reload()
			if err != nil {
				log.Printf("Failed to reload gateway certificate err=%q", err)
			} else if changed {
				log.Printf("Reloaded gateway certificate notAfter=%s", r.holder.Load().Leaf.NotAfter.Format(time.RFC3339))
			}
		}
	}
}
