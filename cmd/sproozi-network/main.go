// Command sproozi-network owns a dedicated Docker network namespace. Only this
// trusted anchor has NET_ADMIN; the native container has no capabilities and
// always runs as UID 1000. It permits that UID to reach one gateway socket only.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const chain = "SPROOZI_NATIVE"
const ipv4Table = "iptables"
const ipv6Table = "ip6tables"

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "probe" {
		return probe(args[1:], os.Stdin, os.Stdout)
	}
	check := len(args) == 3 && args[0] == "check"
	if check {
		args = args[1:]
	}
	if len(args) != 2 {
		return errors.New("network anchor requires fixed gateway IPv4 and port")
	}
	ip, err := netip.ParseAddr(args[0])
	port, portErr := strconv.ParseUint(args[1], 10, 16)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || portErr != nil || port == 0 {
		return errors.New("invalid network anchor gateway")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !check {
		if err = initialize(ctx, ip.String(), strconv.FormatUint(port, 10)); err != nil {
			return err
		}
	}
	if err = verify(ctx, ip.String(), strconv.FormatUint(port, 10)); err != nil {
		return err
	}
	if check {
		return nil
	}
	stopped, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-stopped.Done()
	return nil
}

func initialize(ctx context.Context, ip, port string) error {
	commands := []struct {
		binary string
		args   []string
	}{
		{ipv4Table, []string{"-N", chain}},
		{ipv4Table, []string{"-A", chain, "-p", "tcp", "-d", ip, "--dport", port, "-j", "ACCEPT"}},
		{ipv4Table, []string{"-A", chain, "-j", "REJECT", "--reject-with", "icmp-admin-prohibited"}},
		{ipv6Table, []string{"-N", chain}},
		{ipv6Table, []string{"-A", chain, "-j", "REJECT", "--reject-with", "icmp6-adm-prohibited"}},
		{ipv4Table, []string{"-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "1000", "-j", chain}},
		{ipv6Table, []string{"-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "1000", "-j", chain}},
	}
	for _, command := range commands {
		if _, err := table(ctx, command.binary, command.args...); err != nil {
			return err
		}
	}
	return nil
}

func verify(ctx context.Context, ip, port string) error {
	wanted := map[string][]string{
		ipv4Table: {"-N " + chain,
			"-A " + chain + " -d " + ip + "/32 -p tcp -m tcp --dport " + port + " -j ACCEPT",
			"-A " + chain + " -j REJECT --reject-with icmp-admin-prohibited"},
		ipv6Table: {"-N " + chain, "-A " + chain + " -j REJECT --reject-with icmp6-adm-prohibited"},
	}
	for binary, rules := range wanted {
		data, err := table(ctx, binary, "-S", chain)
		if err != nil || string(bytes.TrimSpace(data)) != strings.Join(rules, "\n") {
			return fmt.Errorf("network anchor %s rules are unavailable or changed", binary)
		}
		data, err = table(ctx, binary, "-S", "OUTPUT")
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) < 2 || lines[1] != "-A OUTPUT -m owner --uid-owner 1000 -j "+chain {
			return fmt.Errorf("network anchor %s first output rule changed", binary)
		}
	}
	return nil
}

func table(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/sbin/"+binary, append([]string{"-w", "2"}, args...)...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/bin", "LC_ALL=C"}
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("network anchor %s operation failed: %w", binary, err)
	}
	return output.Bytes(), nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > (64<<10)-b.Len() {
		return 0, errors.New("network anchor rule listing exceeds limit")
	}
	return b.Buffer.Write(data)
}
