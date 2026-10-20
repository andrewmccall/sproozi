package proxytransport

import (
	"crypto/tls"
	"errors"
	"sync/atomic"
)

// Certificate holds the gateway's current serving certificate. Replacing it
// affects new TLS handshakes only; established connections keep their leaf.
type Certificate struct {
	current atomic.Pointer[tls.Certificate]
}

// NewCertificate returns a holder serving c.
func NewCertificate(c tls.Certificate) *Certificate {
	h := &Certificate{}
	h.Store(c)
	return h
}

// Store publishes c for subsequent handshakes.
func (h *Certificate) Store(c tls.Certificate) { h.current.Store(&c) }

// Load returns the certificate for a new handshake.
func (h *Certificate) Load() *tls.Certificate { return h.current.Load() }

func (h *Certificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := h.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("gateway serving certificate unavailable")
}
