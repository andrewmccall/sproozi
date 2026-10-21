package manifests_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The remote executor's configuration and the CLI's trust anchor for it.
const (
	executorProfileConfigMap = "sproozi-work-profile"
	executorTrustConfigMap   = "sproozi-remote-executor"
	cliServiceAccount        = "sproozi-cli"
)

// The executor's Role may update every ConfigMap in its namespace, for
// registrations, so only immutability stops a compromised executor rewriting
// its own profile (principal, allowances, namespaces, gatewayUser) or the CA
// the CLI trusts for it. The example does not define either ConfigMap; any it
// ever defines, and every documented command that creates one, must make it
// immutable.
func TestExecutorConfigurationConfigMapsAreImmutable(t *testing.T) {
	root := repoRoot(t)
	names := []string{executorProfileConfigMap, executorTrustConfigMap}
	example := filepath.Join(root, "examples", "portable-workflows", "kubernetes")
	for _, doc := range objectsOfKind(renderKustomization(t, example), "ConfigMap") {
		if slices.Contains(names, metadataName(doc)) && doc["immutable"] != true {
			t.Errorf("the executor example defines ConfigMap %s without immutable: true", metadataName(doc))
		}
	}
	owner, err := os.ReadFile(filepath.Join(example, "owner.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(string(owner), name) || !strings.Contains(string(owner), `"immutable: true"`) {
			t.Errorf("owner.yaml does not tell the administrator to create %s immutable", name)
		}
	}
	created := map[string]bool{}
	for _, doc := range []string{
		filepath.Join(root, "cmd", "session-profile", "README.md"),
		filepath.Join(root, "docs", "reference", "remote-executors.md"),
		filepath.Join(root, "docs", "guides", "retained-work-kubernetes.md"),
	} {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		blocks := strings.Split(string(data), "```")
		for i := 1; i < len(blocks); i += 2 {
			for _, name := range names {
				if !strings.Contains(blocks[i], "create configmap "+name) {
					continue
				}
				created[name] = true
				if !strings.Contains(blocks[i], "immutable: true") {
					t.Errorf("%s creates ConfigMap %s without immutable: true:\n%s",
						filepath.Base(doc), name, blocks[i])
				}
			}
		}
	}
	for _, name := range names {
		if !created[name] {
			t.Errorf("no executor document shows how to create ConfigMap %s", name)
		}
	}
}

// The CLI never sends the executor its kubeconfig credential: it mints a
// short-lived, audience-bound token for sproozi-cli, and for no other
// ServiceAccount (docs/reference/remote-executors.md).
func TestCLIMayMintOnlyItsOwnExecutorToken(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "config", "cli-access"))
	minted := false
	for _, role := range objectsOfKind(docs, "Role") {
		if metadataName(role) != cliServiceAccount {
			continue
		}
		for _, rule := range decodeRules(t, role) {
			if !slices.Contains(rule.Resources, "serviceaccounts/token") && !slices.Contains(rule.Resources, "*") {
				continue
			}
			if !slices.Equal(rule.ResourceNames, []string{cliServiceAccount}) {
				t.Errorf("Role %s may mint tokens for %v; want only %s", cliServiceAccount, rule.ResourceNames,
					cliServiceAccount)
			}
			minted = minted || slices.Contains(rule.Verbs, verbCreate)
		}
	}
	if !minted {
		t.Errorf("Role %s cannot create serviceaccounts/token for %s, which reaching the remote executor needs",
			cliServiceAccount, cliServiceAccount)
	}
}
