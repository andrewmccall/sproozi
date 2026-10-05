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
	"fmt"
	"net"
	"strconv"
	"strings"
)

// parseAuthority splits authority (host or host:port) into a normalised
// lowercase hostname and integer port. defaultPort is used when no port is
// present. Returns an error for any form rejected by policy.
func parseAuthority(authority, defaultPort string) (string, int, error) {
	rawHost, portStr, err := net.SplitHostPort(authority)
	if err != nil {
		rawHost = authority
		portStr = defaultPort
	}

	// Reject IPv6 bracket literals (authority started with '[').
	if strings.HasPrefix(authority, "[") {
		return "", 0, fmt.Errorf("egress: IP literal addresses are not permitted")
	}
	// Reject IPv4 and bare IPv6.
	if net.ParseIP(rawHost) != nil {
		return "", 0, fmt.Errorf("egress: IP literal addresses are not permitted")
	}
	// Reject trailing dot (DNS root label bypass).
	if strings.HasSuffix(rawHost, ".") {
		return "", 0, fmt.Errorf("egress: trailing dot in hostname is not permitted")
	}
	// Reject percent-encoded characters.
	if strings.Contains(rawHost, "%") {
		return "", 0, fmt.Errorf("egress: percent-encoded characters in hostname are not permitted")
	}
	// Reject userinfo (defensive).
	if strings.Contains(rawHost, "@") {
		return "", 0, fmt.Errorf("egress: userinfo in hostname is not permitted")
	}

	p, parseErr := strconv.Atoi(portStr)
	if parseErr != nil || p <= 0 || p > 65535 {
		return "", 0, fmt.Errorf("egress: invalid port %q", portStr)
	}

	return strings.ToLower(rawHost), p, nil
}

// isApproved reports whether (scheme, host, port) exactly matches any entry.
func isApproved(scheme, host string, port int, dests []Destination) bool {
	for _, d := range dests {
		if d.Scheme == scheme && d.Host == host && d.Port == port {
			return true
		}
	}
	return false
}
