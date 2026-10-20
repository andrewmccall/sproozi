package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// contractVersion names the probe and anchor interface local setup relies on.
// Setup rejects an image whose helper reports a different version.
const contractVersion = "1"

const (
	probeReach = "reach"
	probeTLS   = "tls"
	tcp4       = "tcp4"
)

// probe runs one diagnostic inside a worker network namespace. None of these
// change firewall state; the anchor alone does.
//
//	contract                       print the helper contract version
//	reach HOST PORT NONCE          connect and expect NONCE; print the IPv4 used
//	tls IP PORT < trust.pem        TLS 1.3 handshake verifying the gateway IP SAN
//	deny IP PORT                   attempt forbidden paths; print a JSON report
func probe(args []string, stdin io.Reader, stdout io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "contract":
		_, err := fmt.Fprintln(stdout, contractVersion)
		return err
	case len(args) == 4 && args[0] == probeReach:
		return reach(args[1], args[2], args[3], stdout)
	case len(args) == 3 && args[0] == probeTLS:
		return verifyTLS(args[1], args[2], stdin)
	case len(args) == 3 && args[0] == "deny":
		return deny(args[1], args[2], stdout)
	}
	return errors.New("usage: sproozi-network probe contract | reach HOST PORT NONCE | tls IP PORT | deny IP PORT")
}

func reach(host, port, nonce string, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addresses) == 0 {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, address := range addresses {
		target := net.JoinHostPort(address.String(), port)
		conn, err := (&net.Dialer{}).DialContext(ctx, tcp4, target)
		if err != nil {
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, len(nonce))
		_, err = io.ReadFull(conn, got)
		_ = conn.Close()
		if err == nil && string(got) == nonce {
			_, err = fmt.Fprintln(stdout, address.String())
			return err
		}
	}
	return fmt.Errorf("no address for %s reached the setup listener on port %s", host, port)
}

func verifyTLS(ip, port string, stdin io.Reader) error {
	trust, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return errors.New("trust bundle has no certificates")
	}
	if _, err = netip.ParseAddr(ip); err != nil {
		return errors.New("gateway IP required")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	config := &tls.Config{RootCAs: roots, ServerName: ip, MinVersion: tls.VersionTLS13}
	conn, err := tls.DialWithDialer(dialer, tcp4, net.JoinHostPort(ip, port), config)
	if err != nil {
		return fmt.Errorf("gateway TLS verification failed: %w", err)
	}
	return conn.Close()
}

// denial is one forbidden path. Reached reports an unexpected success.
type denial struct {
	Path    string `json:"path"`
	Reached bool   `json:"reached"`
	Error   string `json:"error,omitempty"`
}

// deny attempts paths the anchor must block for UID 1000: a public address,
// the gateway on another port, IPv6, cloud metadata and direct DNS. It fails
// when any attempt connects.
func deny(ip, port string, stdout io.Writer) error {
	other, err := strconv.Atoi(port)
	if err != nil {
		return errors.New("gateway port required")
	}
	other = other%65535 + 1
	attempts := []struct{ network, address, path string }{
		{tcp4, "1.1.1.1:443", "public IPv4 HTTPS"},
		{tcp4, "api.openai.com:443", "direct OpenAI provider"},
		{tcp4, "api.anthropic.com:443", "direct Anthropic provider"},
		{tcp4, net.JoinHostPort(ip, strconv.Itoa(other)), "gateway alternate port"},
		{tcp4, "169.254.169.254:80", "metadata endpoint"},
		{"tcp6", "[2606:4700:4700::1111]:443", "public IPv6 HTTPS"},
		{"udp4", "1.1.1.1:53", "direct DNS"},
	}
	var report []denial
	failed := false
	for _, a := range attempts {
		d := denial{Path: a.path}
		d.Reached, err = attempt(a.network, a.address)
		if err != nil {
			d.Error = err.Error()
		}
		failed = failed || d.Reached
		report = append(report, d)
	}
	if err = json.NewEncoder(stdout).Encode(report); err != nil {
		return err
	}
	if failed {
		return errors.New("a forbidden network path was reachable")
	}
	return nil
}

func attempt(network, address string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	if network != "udp4" {
		return true, nil
	}
	// UDP "connects" locally; a reply proves the path. Send a DNS query.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	query := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0,
		7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	if _, err = conn.Write(query); err != nil {
		return false, err
	}
	reply := make([]byte, 512)
	if _, err = conn.Read(reply); err != nil {
		return false, err
	}
	return true, nil
}
