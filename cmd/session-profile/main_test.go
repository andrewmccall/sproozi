package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
)

func TestServerConfigurationRequiresExplicitSupportedNativeKind(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude, "", "unknown"} {
		t.Run(string(kind), func(t *testing.T) {
			data, err := json.Marshal(serverConfig{Harness: kind, Listen: "127.0.0.1:443",
				CertificateFile: "/fixed/cert", KeyFile: "/fixed/key"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readConfig(path)
			if kind == harness.NativeCodex || kind == harness.NativeClaude {
				if err != nil || got.Harness != kind {
					t.Fatalf("explicit kind: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("missing/unknown kind silently selected Codex")
			}
		})
	}
}

const testBudgetNamespace = "sproozi-budget"

func validServerConfig() serverConfig {
	return serverConfig{Harness: harness.NativeCodex, Listen: "0.0.0.0:8443", CertificateFile: "/fixed/cert",
		KeyFile: "/fixed/key", Principal: "developer", PlacementName: "engineering", Namespace: "sproozi-sessions",
		BudgetNamespace: testBudgetNamespace, TemplateName: "mobility-native", Directory: "/state/profile",
		InstructionDigest: strings.Repeat("ab", 32),
		Allowances: map[string]session.Allowance{"model.inference": {
			Units: session.Limit{Bounded: true, Value: 200000}, CostMicros: session.Limit{}}},
		MaxDuration: "30m", MaxCheckpointBytes: 8 << 20}
}

// Each invalid setting is reported by its configuration field name.
func TestValidateConfigNamesTheBadField(t *testing.T) {
	if err := validateConfig(validServerConfig()); err != nil {
		t.Fatalf("valid configuration: %v", err)
	}
	for field, change := range map[string]func(*serverConfig){
		"principal":          func(c *serverConfig) { c.Principal = "" },
		"placementName":      func(c *serverConfig) { c.PlacementName = "" },
		fieldNamespace:       func(c *serverConfig) { c.Namespace = "Not_A_Namespace" },
		fieldBudgetNamespace: func(c *serverConfig) { c.BudgetNamespace = "Bad_Budget" },
		"gatewayUser":        func(c *serverConfig) { c.GatewayUser = "sproozi-shared-gateway" },
		"templateName":       func(c *serverConfig) { c.TemplateName = "" },
		"directory":          func(c *serverConfig) { c.Directory = "relative/state" },
		"instructionDigest":  func(c *serverConfig) { c.InstructionDigest = strings.Repeat("AB", 32) },
		"allowances":         func(c *serverConfig) { c.Allowances = nil },
		"maxDuration":        func(c *serverConfig) { c.MaxDuration = "-5m" },
		"maxCheckpointBytes": func(c *serverConfig) { c.MaxCheckpointBytes = 0 },
		"workTokenFile": func(c *serverConfig) {
			c.Repositories = map[string]work.Repository{"app": {URL: "https://github.com/example/app"}}
		},
	} {
		t.Run(field, func(t *testing.T) {
			c := validServerConfig()
			change(&c)
			err := validateConfig(c)
			if err == nil || !strings.Contains(err.Error(), `"`+field+`"`) && !strings.Contains(err.Error(), field+" ") {
				t.Fatalf("validateConfig = %v, want the field %q named", err, field)
			}
		})
	}
	c := validServerConfig()
	c.Allowances["model.inference"] = session.Allowance{Units: session.Limit{Bounded: true, Value: 1},
		CostMicros: session.Limit{Bounded: true, Value: 0}}
	if err := validateConfig(c); err == nil || !strings.Contains(err.Error(), `"model.inference"`) {
		t.Fatalf("validateConfig = %v, want the allowance named", err)
	}
}

func TestReadConfigNamesTheMissingListenerField(t *testing.T) {
	for _, field := range []string{fieldListen, fieldCertificateFile, fieldKeyFile} {
		t.Run(field, func(t *testing.T) {
			c := validServerConfig()
			switch field {
			case fieldListen:
				c.Listen = ""
			case fieldCertificateFile:
				c.CertificateFile = ""
			case fieldKeyFile:
				c.KeyFile = ""
			}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = readConfig(path); err == nil || !strings.Contains(err.Error(), `"`+field+`"`) {
				t.Fatalf("readConfig = %v, want %q named", err, field)
			}
		})
	}
}
