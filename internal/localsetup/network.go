package localsetup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// Network is the installation-owned Docker bridge network.
type Network struct {
	Name    string `json:"name"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
}

const installationLabel = "sproozi.com/installation"

// EnsureNetwork creates or reuses the installation's bridge network. Docker
// selects a subnet that does not overlap its other networks. A same-named
// network owned by anything else is never adopted.
func EnsureNetwork(ctx context.Context, d Docker, installation string) (Network, error) {
	name := "sproozi-" + installation
	n, err := inspectNetwork(ctx, d, name, installation)
	if err == nil {
		return n, nil
	}
	if !strings.Contains(err.Error(), "not found") {
		return Network{}, err
	}
	if _, err = d.Output(ctx, nil, "network", "create", "--driver", "bridge", "--label", installationLabel+"="+installation, name); err != nil {
		return Network{}, fail("network", fmt.Errorf("create %s: %w", name, err), "check Docker address pools with docker network ls")
	}
	return inspectNetwork(ctx, d, name, installation)
}

func inspectNetwork(ctx context.Context, d Docker, name, installation string) (Network, error) {
	out, err := d.Output(ctx, nil, "network", "inspect", name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return Network{}, fmt.Errorf("network %s not found", name)
		}
		return Network{}, fail("network", err, "")
	}
	var found []struct {
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
		IPAM   struct {
			Config []struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err = json.Unmarshal([]byte(out), &found); err != nil || len(found) != 1 {
		return Network{}, fail("network", fmt.Errorf("unreadable network %s", name), "")
	}
	f := found[0]
	if f.Driver != "bridge" || f.Labels[installationLabel] != installation {
		return Network{}, fail("network", fmt.Errorf("network %s exists but is not owned by this installation", name), "remove it or choose another installation root")
	}
	n := Network{Name: name}
	for _, c := range f.IPAM.Config {
		if prefix, err := netip.ParsePrefix(c.Subnet); err == nil && prefix.Addr().Is4() {
			n.Subnet, n.Gateway = c.Subnet, c.Gateway
		}
	}
	if n.Subnet == "" {
		return Network{}, fail("network", fmt.Errorf("network %s has no IPv4 subnet", name), "")
	}
	return n, nil
}
