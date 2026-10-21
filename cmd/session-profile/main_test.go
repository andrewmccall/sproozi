package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
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
