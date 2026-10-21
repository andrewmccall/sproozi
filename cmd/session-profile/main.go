// Command session-profile serves one administrator-owned Kubernetes placement.
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/placement"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/session/remote"
	"github.com/andrewmccall/sproozi/internal/work"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type serverConfig struct {
	Harness            harness.NativeKind                `json:"harness"`
	Listen             string                            `json:"listen"`
	CertificateFile    string                            `json:"certificateFile"`
	KeyFile            string                            `json:"keyFile"`
	Principal          string                            `json:"principal"`
	PlacementName      string                            `json:"placementName"`
	Namespace          string                            `json:"namespace"`
	BudgetNamespace    string                            `json:"budgetNamespace,omitempty"`
	GatewayUser        string                            `json:"gatewayUser,omitempty"`
	TemplateName       string                            `json:"templateName"`
	Directory          string                            `json:"directory"`
	InstructionDigest  string                            `json:"instructionDigest"`
	Allowances         map[string]session.Allowance      `json:"allowances"`
	MaxDuration        string                            `json:"maxDuration"`
	MaxCheckpointBytes int64                             `json:"maxCheckpointBytes"`
	WorkTokenFile      string                            `json:"workTokenFile,omitempty"`
	Repositories       map[string]work.Repository        `json:"repositories,omitempty"`
	Projects           map[project.ID]project.Definition `json:"projects,omitempty"`
}

// Configuration field names, as errors name them.
const (
	fieldListen          = "listen"
	fieldCertificateFile = "certificateFile"
	fieldKeyFile         = "keyFile"
	fieldNamespace       = "namespace"
	fieldBudgetNamespace = "budgetNamespace"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Session profile failed:", err)
		os.Exit(1)
	}
}
func readConfig(path string) (serverConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return serverConfig{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return serverConfig{}, errors.New("configuration exceeds bound")
	}
	var value serverConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		return value, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return value, errors.New("trailing configuration JSON")
	}
	if err = value.Harness.Validate(); err != nil {
		return value, fmt.Errorf("configuration \"harness\": %w", err)
	}
	for _, field := range []struct{ name, value string }{
		{fieldListen, value.Listen}, {fieldCertificateFile, value.CertificateFile}, {fieldKeyFile, value.KeyFile},
	} {
		if field.value == "" {
			return value, fmt.Errorf("configuration %q is required: the executor serves a fixed TLS listener", field.name)
		}
	}
	return value, nil
}

// validateConfig checks the placement settings one field at a time, so a
// mistake names the field to fix instead of failing as one invalid
// configuration. placement.Open checks the same rules again.
func validateConfig(c serverConfig) error {
	invalid := func(field, rule string) error {
		return fmt.Errorf("configuration %q %s", field, rule)
	}
	if c.Principal == "" {
		return invalid("principal", "is required")
	}
	if c.PlacementName == "" {
		return invalid("placementName", "is required")
	}
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return invalid(fieldNamespace, "must be a Kubernetes namespace name")
	}
	if c.BudgetNamespace != "" && len(validation.IsDNS1123Label(c.BudgetNamespace)) != 0 {
		return invalid(fieldBudgetNamespace, "must be a Kubernetes namespace name")
	}
	if c.GatewayUser != "" {
		if _, err := gatewayNamespace(c.GatewayUser); err != nil {
			return err
		}
	}
	if len(validation.IsDNS1123Subdomain(c.TemplateName)) != 0 {
		return invalid("templateName", "must be an AgentTemplate name")
	}
	if !filepath.IsAbs(c.Directory) {
		return invalid("directory", "must be an absolute path")
	}
	if digest, err := hex.DecodeString(c.InstructionDigest); err != nil || len(digest) != 32 ||
		hex.EncodeToString(digest) != c.InstructionDigest {
		return invalid("instructionDigest", "must be 64 lowercase hexadecimal characters (a SHA-256 digest)")
	}
	if len(c.Allowances) == 0 || len(c.Allowances) > 32 {
		return invalid("allowances", "must hold between 1 and 32 capabilities")
	}
	for name, a := range c.Allowances {
		if !a.Units.Bounded || a.Units.Value <= 0 {
			return invalid("allowances", fmt.Sprintf("entry %q needs bounded positive units", name))
		}
		if a.CostMicros.Bounded && a.CostMicros.Value <= 0 || !a.CostMicros.Bounded && a.CostMicros.Value != 0 {
			return invalid("allowances", fmt.Sprintf("entry %q needs costMicros bounded and positive, "+
				"or unbounded with value 0", name))
		}
	}
	if duration, err := time.ParseDuration(c.MaxDuration); err != nil || duration <= 0 {
		return invalid("maxDuration", "must be a positive Go duration such as \"30m\"")
	}
	if c.MaxCheckpointBytes <= 0 || c.MaxCheckpointBytes > 256<<20 {
		return invalid("maxCheckpointBytes", "must be between 1 and 268435456")
	}
	if len(c.Repositories)+len(c.Projects) > 0 && c.WorkTokenFile == "" {
		return invalid("workTokenFile", "is required with repositories or projects")
	}
	return nil
}
func run(args []string) error {
	flags := flag.NewFlagSet("session-profile", flag.ContinueOnError)
	path := flags.String("config", "", "administrator configuration JSON file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return errors.New("use session-profile --config FILE")
	}
	fixed, err := readConfig(*path)
	if err != nil {
		return err
	}
	if err = validateConfig(fixed); err != nil {
		return err
	}
	duration, err := time.ParseDuration(fixed.MaxDuration)
	if err != nil {
		return err
	}
	restConfig, err := config.GetConfig()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err = api.AddToScheme(scheme); err != nil {
		return err
	}
	if err = corev1.AddToScheme(scheme); err != nil {
		return err
	}
	kube, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	clients, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	budgetNamespace := cmp.Or(fixed.BudgetNamespace, placement.DefaultBudgetNamespace)
	if err = preflight(context.Background(), clients, fixed.Namespace, budgetNamespace,
		cmp.Or(fixed.GatewayUser, defaultGatewayUser)); err != nil {
		return err
	}
	backend, err := placement.Open(placement.Config{
		Harness: fixed.Harness, Kube: kube, RESTConfig: restConfig, Namespace: fixed.Namespace,
		BudgetNamespace: fixed.BudgetNamespace, TemplateName: fixed.TemplateName, Principal: fixed.Principal,
		Directory: fixed.Directory, Name: fixed.PlacementName,
		InstructionDigest: fixed.InstructionDigest, Allowances: fixed.Allowances,
		MaxDuration: duration, MaxCheckpointBytes: fixed.MaxCheckpointBytes,
	})
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	home, err := homeAuthorizer(fixed, restConfig)
	if err != nil {
		return err
	}
	handler, err := profile.NewHandler(profile.Config{
		Harness: fixed.Harness, Principal: fixed.Principal, Authorizer: home, Placement: fixed.PlacementName,
		InstructionDigest: fixed.InstructionDigest, Allowances: fixed.Allowances,
		MaxDuration: duration, MaxCheckpointBytes: fixed.MaxCheckpointBytes, Backend: backend, Profiles: backend,
	})
	if err != nil {
		return err
	}
	if len(fixed.Repositories)+len(fixed.Projects) > 0 {
		owner, openErr := work.Open(work.Config{
			Directory: filepath.Join(fixed.Directory, "work"), Principal: fixed.Principal,
			Harness: fixed.Harness, Placement: fixed.PlacementName,
			InstructionDigest: fixed.InstructionDigest, Allowances: fixed.Allowances,
			MaxDuration: duration, MaxCheckpointBytes: fixed.MaxCheckpointBytes,
			Repositories: fixed.Repositories, TemplateName: fixed.TemplateName, Projects: fixed.Projects,
			Backend: backend, Log: os.Stderr,
		})
		if openErr != nil {
			return openErr
		}
		defer func() { _ = owner.Close() }()
		taskToken, readErr := os.ReadFile(fixed.WorkTokenFile)
		if readErr != nil {
			return readErr
		}
		workHandler, handlerErr := work.NewHandler(owner, strings.TrimSpace(string(taskToken)), home)
		if handlerErr != nil {
			return handlerErr
		}
		profileHandler := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/work/") {
				workHandler.ServeHTTP(w, r)
				return
			}
			profileHandler.ServeHTTP(w, r)
		})
	}
	server := &http.Server{
		Addr: fixed.Listen, Handler: handler, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS(fixed.CertificateFile, fixed.KeyFile) }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		return err
	}
}

// homeAuthorizer decides the home principal: a caller whose Kubernetes
// identity a TokenReview authenticates and RBAC allows to use this executor.
func homeAuthorizer(fixed serverConfig, restConfig *rest.Config) (profile.Authorizer, error) {
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return remote.NewReviewer(kube, fixed.Namespace, fixed.PlacementName)
}
