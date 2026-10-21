package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const fixedToken = "/fixed/token"

func TestServerConfigurationRequiresExplicitSupportedNativeKind(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude, "", "unknown"} {
		t.Run(string(kind), func(t *testing.T) {
			data, err := json.Marshal(serverConfig{Harness: kind, Listen: "127.0.0.1:443",
				CertificateFile: "/fixed/cert", KeyFile: "/fixed/key", TokenFile: fixedToken})
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

func TestServerConfigurationSelectsOneHomeAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, authorization, tokenFile, problem string
	}{
		{"default token", "", fixedToken, ""},
		{"explicit token", homeByToken, fixedToken, ""},
		{"kubernetes", homeByKubernetes, "", ""},
		{"token missing", "", "", "tokenFile: required"},
		{"kubernetes with token", homeByKubernetes, fixedToken, "tokenFile: omit it"},
		{"unknown", "oidc", "", `homeAuthorization: "oidc"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(serverConfig{Harness: harness.NativeCodex, Listen: "127.0.0.1:443",
				CertificateFile: "/fixed/cert", KeyFile: "/fixed/key", TokenFile: tc.tokenFile,
				HomeAuthorization: tc.authorization})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = readConfig(path)
			if tc.problem == "" && err != nil || tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)) {
				t.Fatalf("got %v, want %q", err, tc.problem)
			}
		})
	}
}
