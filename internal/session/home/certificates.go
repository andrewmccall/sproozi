package home

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	"github.com/andrewmccall/sproozi/internal/session"
)

// certificates is the gateway's serving identity and worker trust. With
// gateway.pkiDirectory, the home owns a private CA: it renews the leaf, stages
// CA rotation and records which allocations received which trust generation.
// Otherwise the administrator's fixed files are used unchanged.
type certificates struct {
	store   *pki.Store
	names   pki.Names
	serving *proxytransport.Certificate
	trust   []byte
	log     io.Writer
	// fenced reports a placement's independent stop evidence for a launch.
	fenced func(session.Launch) (bool, error)
	// listeners are the gateway addresses checked after a switch.
	listeners []string
}

// maintenanceInterval also bounds how late renewal notices a sleep/wake jump.
const maintenanceInterval = time.Minute

func openCertificates(c Config, names pki.Names, log io.Writer) (*certificates, error) {
	if c.Gateway.PKIDirectory == "" {
		cert, trust, err := staticCertificate(c.Gateway, names)
		if err != nil {
			return nil, err
		}
		return &certificates{names: names, serving: proxytransport.NewCertificate(cert), trust: trust, log: log}, nil
	}
	if c.Gateway.CertificateFile != "" || c.Gateway.KeyFile != "" || c.Gateway.TrustBundleFile != "" {
		return nil, errors.New("home: choose gateway.pkiDirectory or administrator certificate files, not both")
	}
	store, err := pki.Open(c.Gateway.PKIDirectory, pki.DefaultPolicy, nil)
	if err != nil {
		return nil, err
	}
	if _, err = store.State(); err != nil {
		// Never create a CA implicitly at service start; setup owns creation.
		return nil, err
	}
	if _, err = store.Ensure(names); err != nil {
		return nil, err
	}
	cert, trust, _, err := store.Serving()
	if err != nil {
		return nil, err
	}
	if err = pki.VerifyServing(cert.Leaf, trust, names, time.Now()); err != nil {
		return nil, err
	}
	return &certificates{store: store, names: names, serving: proxytransport.NewCertificate(cert), log: log}, nil
}

func staticCertificate(g GatewayConfig, names pki.Names) (tls.Certificate, []byte, error) {
	var cert tls.Certificate
	key, err := readSetting("gateway.keyFile", g.KeyFile, 64<<10)
	if err != nil {
		return cert, nil, err
	}
	data, err := readSetting("gateway.certificateFile", g.CertificateFile, 64<<10)
	if err != nil {
		return cert, nil, err
	}
	if cert, err = tls.X509KeyPair(data, key); err != nil {
		return cert, nil, keyPairError(g, err)
	}
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return cert, nil, fmt.Errorf("gateway.certificateFile %s: %w", g.CertificateFile, err)
	}
	for _, ip := range names.IPs {
		if cert.Leaf.VerifyHostname(ip) != nil {
			return cert, nil, fmt.Errorf("home: gateway.certificateFile %s does not name local.gatewayIPv4 %s; "+
				"issue it with that address as an IP subject alternative name", g.CertificateFile, ip)
		}
	}
	for _, host := range names.DNS {
		if cert.Leaf.VerifyHostname(host) != nil {
			return cert, nil, fmt.Errorf("home: gateway.certificateFile %s does not name %s, a host the gateway "+
				"inspects; issue it with that name as a DNS subject alternative name", g.CertificateFile, host)
		}
	}
	trust, err := readSetting("gateway.trustBundleFile", g.TrustBundleFile, 64<<10)
	if err != nil {
		return cert, nil, err
	}
	// Administrator files may carry intermediates.
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return cert, nil, fmt.Errorf("gateway.trustBundleFile %s: no PEM CA certificate found", g.TrustBundleFile)
	}
	for _, der := range cert.Certificate[1:] {
		intermediate, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			return cert, nil, fmt.Errorf("gateway.certificateFile %s: %w", g.CertificateFile, parseErr)
		}
		intermediates.AddCert(intermediate)
	}
	for _, name := range append(slices.Clone(names.IPs), names.DNS...) {
		if _, err = cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: name}); err != nil {
			return cert, nil, fmt.Errorf("gateway.certificateFile %s does not verify against gateway.trustBundleFile %s: %w",
				g.CertificateFile, g.TrustBundleFile, err)
		}
	}
	return cert, trust, nil
}

// trustFor returns the certificate-only bundle for a new allocation. In
// managed mode the allocation is recorded as a trust consumer first.
func (c *certificates) trustFor(l session.Launch) ([]byte, error) {
	if c.store == nil {
		return bytes.Clone(c.trust), nil
	}
	reference, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	return c.store.TrustFor(l.AllocationID, reference)
}

// releaseConsumer forgets an allocation's trust record once its usage is
// finalized, which follows the placement's independent stop.
func (c *certificates) releaseConsumer(l session.Launch) {
	if c.store != nil {
		_ = c.store.Release(l.AllocationID)
	}
}

// maintain renews the serving leaf and advances routine CA rotation when the
// recorded consumers allow it. Failures keep the previous valid generation.
func (c *certificates) maintain() error {
	if c.store == nil {
		return nil
	}
	changed, err := c.store.Ensure(c.names)
	if err != nil {
		return err
	}
	st, err := c.store.State()
	if err != nil {
		return err
	}
	if c.store.RotationDue(st) {
		if err = c.store.PrepareRotation(); err != nil {
			return err
		}
		c.logf("Prepared certificate authority rotation")
		if st, err = c.store.State(); err != nil {
			return err
		}
	}
	if st.Rotation != nil {
		distributed, err := c.distributed(st.Rotation.To)
		if err != nil {
			return err
		}
		switch {
		case distributed && st.Rotation.Stage == pki.StagePrepared:
			if err = c.store.SwitchRotation(true); err != nil {
				return err
			}
			changed = true
			c.logf("Switched gateway certificate to the new authority")
		case distributed && st.Rotation.Stage == pki.StageSwitched:
			// Retire only after the gateway serves a leaf the new trust accepts.
			if err = c.selfCheck(); err != nil {
				return fmt.Errorf("switched gateway certificate failed its check; the previous authority stays trusted: %w", err)
			}
			if err = c.store.RetireRotation(true); err != nil {
				return err
			}
			c.logf("Retired previous certificate authority")
		}
	}
	if !changed {
		// Another certificate owner, such as sproozi certificates renew, may
		// have committed a new leaf.
		current, _, _, err := c.store.Serving()
		if err != nil {
			return err
		}
		changed = !current.Leaf.Equal(c.serving.Load().Leaf)
	}
	if changed {
		return c.reload()
	}
	return nil
}

// distributed releases consumers that independently stopped and reports
// whether every remaining consumer trusts generation.
func (c *certificates) distributed(generation int) (bool, error) {
	lacking, err := c.store.Lacking(generation)
	if err != nil {
		return false, err
	}
	remaining := 0
	for _, consumer := range lacking {
		var l session.Launch
		stopped := false
		if json.Unmarshal(consumer.Reference, &l) == nil && c.fenced != nil {
			stopped, _ = c.fenced(l)
		}
		if stopped {
			if err = c.store.Release(consumer.ID); err != nil {
				return false, err
			}
			continue
		}
		remaining++
	}
	return remaining == 0, nil
}

// selfCheck connects to each gateway listener and verifies the served leaf
// against the current worker trust for the gateway address.
func (c *certificates) selfCheck() error {
	_, trust, _, err := c.store.Serving()
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) || len(c.names.IPs) == 0 {
		return errors.New("no trust or gateway address to check")
	}
	for _, listen := range c.listeners {
		host, port, err := net.SplitHostPort(listen)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp4", net.JoinHostPort(host, port),
			&tls.Config{RootCAs: roots, ServerName: c.names.IPs[0], MinVersion: tls.VersionTLS13})
		if err != nil {
			return err
		}
		_ = conn.Close()
	}
	return nil
}

func (c *certificates) reload() error {
	cert, trust, _, err := c.store.Serving()
	if err != nil {
		return err
	}
	if err = pki.VerifyServing(cert.Leaf, trust, c.names, time.Now()); err != nil {
		return err
	}
	c.serving.Store(cert)
	c.logf("Reloaded gateway serving certificate")
	return nil
}

// run maintains managed certificates until ctx ends.
func (c *certificates) run(ctx context.Context) {
	if c == nil || c.store == nil {
		return
	}
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.maintain(); err != nil {
				c.logf("Failed to maintain gateway certificate err=%q", err)
			}
		}
	}
}

func (c *certificates) logf(format string, args ...any) {
	if c.log != nil {
		_, _ = fmt.Fprintf(c.log, format+"\n", args...)
	}
}
