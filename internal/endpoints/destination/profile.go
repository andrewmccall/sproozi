/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package destination

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

const (
	httpsScheme = "https"
)

// Destination is an exact (scheme, host, port) tuple defining one approved egress target.
type Destination struct {
	Scheme string `json:"scheme"` // "http" or httpsScheme
	Host   string `json:"host"`   // exact lowercase hostname, no wildcards
	Port   int    `json:"port"`
}

// ProfileStore holds administrator-defined named egress profiles.
// Profiles are static gateway configuration — not CRDs.
type ProfileStore struct {
	profiles map[string][]Destination
}

// LoadProfileStore parses a JSON profile configuration map.
//
// Expected format:
//
//	{"npm-registry":[{"scheme":httpsScheme,"host":"registry.npmjs.org","port":443}]}
func LoadProfileStore(data []byte) (*ProfileStore, error) {
	raw := make(map[string][]Destination)
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("egress: failed to parse profile config: %w", err)
	}
	for name, dests := range raw {
		for i, d := range dests {
			if d.Scheme != httpsScheme {
				return nil, fmt.Errorf("egress: profile %q entry %d: invalid scheme %q", name, i, d.Scheme)
			}
			if host, port, err := parseAuthority(net.JoinHostPort(d.Host, strconv.Itoa(d.Port)), "443"); err != nil || host != d.Host || port != d.Port || d.Host == "" {
				return nil, fmt.Errorf("egress: profile %q entry %d: empty host", name, i)
			}
			if d.Port <= 0 || d.Port > 65535 {
				return nil, fmt.Errorf("egress: profile %q entry %d: invalid port %d", name, i, d.Port)
			}
		}
	}
	return &ProfileStore{profiles: raw}, nil
}

// ActiveDestinations returns the union of destinations from profiles that are
// both selected by templateProfiles and approved by policyProfiles.
func (s *ProfileStore) ActiveDestinations(templateProfiles, policyProfiles []string) []Destination {
	var dests []Destination
	for _, name := range templateProfiles {
		if slices.Contains(policyProfiles, name) {
			dests = append(dests, s.profiles[name]...)
		}
	}
	return dests
}

// Destinations returns the configured destination set for routing and TLS setup.
// A route is not a grant: the handler still intersects template and live policy.
func (s *ProfileStore) Destinations() []Destination {
	seen := map[Destination]bool{}
	for _, ds := range s.profiles {
		for _, d := range ds {
			seen[d] = true
		}
	}
	result := make([]Destination, 0, len(seen))
	for d := range seen {
		result = append(result, d)
	}
	slices.SortFunc(result, func(a, b Destination) int {
		if a.Host == b.Host {
			return cmp.Compare(a.Port, b.Port)
		}
		return strings.Compare(a.Host, b.Host)
	})
	return result
}
