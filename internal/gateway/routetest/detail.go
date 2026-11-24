package routetest

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// HandlerDetail describes what a model handler was built with, read from its
// fields: each model handler's protocol, upstream origin, credential type and
// token headers, and whether it has pricing, a drain, diagnostics, a path map
// and the client's headers; and each relay's service, upstream, credential
// and route count. A route record carries it, so a change to the credential
// or upstream a route gets shows in the record. It never reads a credential's
// value.
func HandlerDetail(h http.Handler) string {
	if h == nil {
		return "-"
	}
	var parts []string
	var walk func(v reflect.Value, depth int)
	walk = func(v reflect.Value, depth int) {
		if depth > 4 {
			return
		}
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct || !strings.HasSuffix(v.Type().PkgPath(), "internal/endpoints/model") {
			return
		}
		switch v.Type().Name() {
		case "Handler":
			parts = append(parts, fmt.Sprintf("handler{protocol=%d %s}", v.FieldByName("protocol").Uint(),
				handlerConfig(v.FieldByName("Config"))))
			return
		case "Relay":
			parts = append(parts, fmt.Sprintf("relay{service=%s upstream=%s auth=%s routes=%d}",
				v.FieldByName("Service").String(), v.FieldByName("UpstreamURL").String(),
				credential(v.FieldByName("Auth")), v.FieldByName("Routes").Len()))
			return
		}
		for i := range v.NumField() {
			walk(v.Field(i), depth+1)
		}
	}
	walk(reflect.ValueOf(h), 0)
	return strings.Join(parts, " ")
}

func handlerConfig(c reflect.Value) string {
	return fmt.Sprintf("upstream=%s auth=%s pricing=%t drain=%t diagnostics=%t path=%t keepHeaders=%t",
		c.FieldByName("UpstreamURL").String(), credential(c.FieldByName("ProviderAuth")),
		!c.FieldByName("Pricing").IsNil(), !c.FieldByName("Drain").IsNil(), !c.FieldByName("Diagnostics").IsNil(),
		!c.FieldByName("UpstreamPath").IsNil(), c.FieldByName("KeepClientHeaders").Bool())
}

// credential names an authenticator's type and, for a token, the headers it
// sets, never its value.
func credential(auth reflect.Value) string {
	if auth.IsNil() {
		return "none"
	}
	concrete := auth.Elem()
	name := concrete.Type().String()
	for concrete.Kind() == reflect.Pointer {
		concrete = concrete.Elem()
	}
	if concrete.Kind() == reflect.Struct {
		if headers := concrete.FieldByName("Headers"); headers.IsValid() && headers.Kind() == reflect.Map {
			var pairs []string
			for _, k := range headers.MapKeys() {
				pairs = append(pairs, k.String()+":"+headers.MapIndex(k).String())
			}
			slices.Sort(pairs)
			name += "[" + strings.Join(pairs, ",") + "]"
		}
	}
	return name
}

// ModelRouteLines describes every model.inference route and path route, and
// the route of an authority that carries a model.inference path route, with
// what each handler was built with.
func ModelRouteLines(routes gateway.Dispatcher) []string {
	var lines []string
	describe := func(name string, r gateway.Route) {
		handler := "none"
		if r.Handler != nil {
			handler = fmt.Sprintf("%T", r.Handler)
		}
		capability := string(r.Capability)
		if capability == "" {
			capability = "-"
		}
		lines = append(lines, strings.Join([]string{name, capability, string(r.Tier), handler,
			fmt.Sprintf("providerCredential=%t", r.ProviderCredential),
			fmt.Sprintf("budgets=%t", r.Budgets != nil), fmt.Sprintf("audit=%t", r.AuditLogger != nil),
			HandlerDetail(r.Handler)}, " | "))
	}
	for _, authority := range slices.Sorted(maps.Keys(routes)) {
		route := routes[authority]
		modelPaths := slices.ContainsFunc(route.Paths, func(p gateway.PathRoute) bool {
			return p.Route.Capability == api.CapabilityModelInference
		})
		if route.Capability == api.CapabilityModelInference || modelPaths {
			describe(authority, route)
		}
		for _, p := range route.Paths {
			if p.Route.Capability == api.CapabilityModelInference {
				describe(authority+p.Prefix, p.Route)
			}
		}
	}
	return lines
}

// Grouped is a record that prints each distinct block once, after every
// configuration that produced it, in the order the blocks first appear.
type Grouped struct {
	order   []string
	configs map[string][]string
}

// Add records that config produced block.
func (g *Grouped) Add(config string, block []string) {
	if g.configs == nil {
		g.configs = map[string][]string{}
	}
	key := strings.Join(block, "\n")
	if _, seen := g.configs[key]; !seen {
		g.order = append(g.order, key)
	}
	g.configs[key] = append(g.configs[key], config)
}

// Lines are the record's lines: each block after a "config:" line for every
// configuration that produced it.
func (g *Grouped) Lines() []string {
	var lines []string
	for _, key := range g.order {
		for _, config := range g.configs[key] {
			lines = append(lines, "config: "+config)
		}
		lines = append(lines, strings.Split(key, "\n")...)
	}
	return lines
}
