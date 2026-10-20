package localsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// candidate is one way a worker on the owned network may reach the host:
// the host bind for the gateway listener and the name or IPv4 workers dial.
type candidate struct {
	listen, target, method string
}

// candidates prefer the narrowest bind. Docker Desktop forwards its host
// address to host loopback; Docker Engine on Linux reaches the bridge gateway
// address assigned to the host. A wildcard bind is the last resort.
func candidates(n Network) []candidate {
	return []candidate{
		{"127.0.0.1", "host.docker.internal", "engine host forwarding to loopback"},
		{n.Gateway, n.Gateway, "bridge gateway address"},
		{"0.0.0.0", "host.docker.internal", "engine host forwarding, all interfaces"},
		{"0.0.0.0", n.Gateway, "bridge gateway, all interfaces"},
	}
}

// SelectEndpoint finds a host bind and the IPv4 a worker on the owned network
// actually reaches, by serving a nonce and probing from a container on that
// network. Nothing is assumed about a platform's host address.
func SelectEndpoint(ctx context.Context, d Docker, image Image, n Network, port uint16) (Endpoint, error) {
	var attempts []string
	for _, c := range candidates(n) {
		if c.listen == "" || c.target == "" {
			continue
		}
		ip, err := probeReach(ctx, d, image, n, c, port)
		if err == nil {
			return Endpoint{GatewayIPv4: ip, ListenHost: c.listen, Method: c.method}, nil
		}
		attempts = append(attempts, fmt.Sprintf("%s via %s: %v", c.target, c.listen, err))
	}
	return Endpoint{}, fail("network", fmt.Errorf("no host address is reachable from the worker network: %s", strings.Join(attempts, "; ")), "check Docker networking or firewall rules for port "+fmt.Sprint(port))
}

func probeReach(ctx context.Context, d Docker, image Image, n Network, c candidate, port uint16) (string, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort(c.listen, fmt.Sprint(port)))
	if err != nil {
		return "", fmt.Errorf("port unavailable: %w", err)
	}
	defer func() { _ = listener.Close() }()
	nonce := randomHex(16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(nonce))
			_ = conn.Close()
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := d.Output(probeCtx, nil, append(workerFlags("--network", n.Name), image.ID, "probe", "reach", c.target, fmt.Sprint(port), nonce)...)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if addr, err := netip.ParseAddr(ip); err != nil || !addr.Is4() || addr.IsUnspecified() {
		return "", fmt.Errorf("probe returned %q", ip)
	}
	return ip, nil
}

// PortFree reports whether the host can bind host:port now.
func PortFree(host string, port uint16) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// workerFlags are the restricted worker settings: non-root, no capabilities,
// read-only root and no privilege escalation.
func workerFlags(network ...string) []string {
	flags := []string{"run", "--rm", "-i", "--user", "1000:1000", "--cap-drop", "ALL", "--read-only",
		"--security-opt", "no-new-privileges", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216",
		"--entrypoint", "/usr/local/bin/sproozi-network"}
	return append(flags, network...)
}

// PathReport is the result of probing the actual restricted worker path.
type PathReport struct {
	Gateway string    `json:"gateway"`
	TLS     bool      `json:"tls"`
	Denied  []Denial  `json:"denied,omitempty"`
	Checked time.Time `json:"checked"`
}

// Denial is one forbidden path and whether it was (wrongly) reachable.
type Denial struct {
	Path    string `json:"path"`
	Reached bool   `json:"reached"`
}

// ProbePath starts a real network anchor for ip:port on the owned network,
// joins a restricted worker to its namespace and verifies the gateway's TLS
// identity against trust, then attempts forbidden paths. It removes both
// containers afterwards.
func ProbePath(ctx context.Context, d Docker, installation string, image Image, n Network, ip string, port uint16, trust []byte) (PathReport, error) {
	report := PathReport{Gateway: net.JoinHostPort(ip, fmt.Sprint(port)), Checked: time.Now().UTC()}
	anchor := "sproozi-" + installation + "-probe-" + randomHex(4)
	_, err := d.Output(ctx, nil, "run", "-d", "--name", anchor, "--label", installationLabel+"="+installation, "--label", "sproozi.com/role=probe",
		"--network", n.Name, "--user", "0:0", "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--read-only",
		"--security-opt", "no-new-privileges", "--entrypoint", "/usr/local/bin/sproozi-network", image.ID, ip, fmt.Sprint(port))
	if err != nil {
		return report, fail("network", fmt.Errorf("network anchor did not start: %w", err), "")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_, _ = d.Output(cleanup, nil, "rm", "-f", anchor)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err = d.Output(ctx, nil, "exec", anchor, "/usr/local/bin/sproozi-network", "check", ip, fmt.Sprint(port))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return report, fail("network", fmt.Errorf("network anchor rules were not installed: %w", err), "")
		}
		time.Sleep(250 * time.Millisecond)
	}
	worker := workerFlags("--network", "container:"+anchor)
	if _, err = d.Output(ctx, bytes.NewReader(trust), append(worker, image.ID, "probe", "tls", ip, fmt.Sprint(port))...); err != nil {
		return report, fail("tls", fmt.Errorf("gateway TLS from the restricted worker path failed: %w", err), "run sproozi doctor")
	}
	report.TLS = true
	out, err := d.Output(ctx, nil, append(worker, image.ID, "probe", "deny", ip, fmt.Sprint(port))...)
	var denials []Denial
	if jsonErr := json.Unmarshal([]byte(out), &denials); jsonErr != nil {
		return report, fail("network", errors.New("denial probe produced no report"), "")
	}
	report.Denied = denials
	if err != nil {
		return report, fail("network", errors.New("a forbidden path was reachable from the restricted worker"), "do not use this engine until containment is fixed")
	}
	return report, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
