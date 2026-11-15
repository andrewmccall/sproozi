package main

import (
	"crypto/x509"
	"errors"
	"net/url"
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativeproxy"
)

// startGatewayForwarder starts the loopback relay for a projected gateway
// binding and returns its proxy URL. A launch without one (Docker, whose
// gateway grant does not expire and whose network anchor admits only the
// gateway) needs no forwarder.
func startGatewayForwarder(gateway *harness.NativeGateway) (*nativeproxy.Forwarder, string, error) {
	if gateway == nil {
		return nil, "", nil
	}
	if err := gateway.Validate(nil); err != nil {
		return nil, "", err
	}
	endpoint, err := url.Parse(gateway.Endpoint)
	if err != nil {
		return nil, "", err
	}
	bundle, err := os.ReadFile(gateway.TrustBundle)
	if err != nil {
		return nil, "", errors.New("projected gateway trust bundle is unavailable")
	}
	trust := x509.NewCertPool()
	if !trust.AppendCertsFromPEM(bundle) {
		return nil, "", errors.New("projected gateway trust bundle has no certificates")
	}
	forwarder, err := nativeproxy.Start(nativeproxy.Config{Gateway: endpoint, TokenPath: gateway.TokenPath, Trust: trust})
	if err != nil {
		return nil, "", err
	}
	return forwarder, forwarder.ProxyURL(), nil
}

func closeForwarder(forwarder *nativeproxy.Forwarder) {
	if forwarder != nil {
		_ = forwarder.Close()
	}
}
