// Command session-profile serves one administrator-owned Kubernetes placement.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"github.com/andrewmccall/sproozi/internal/work"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type serverConfig struct {
	Harness            harness.NativeKind                `json:"harness"`
	Listen             string                            `json:"listen"`
	CertificateFile    string                            `json:"certificateFile"`
	KeyFile            string                            `json:"keyFile"`
	TokenFile          string                            `json:"tokenFile"`
	Principal          string                            `json:"principal"`
	PlacementName      string                            `json:"placementName"`
	Namespace          string                            `json:"namespace"`
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
	if value.Harness.Validate() != nil || value.Listen == "" || value.CertificateFile == "" || value.KeyFile == "" {
		return value, errors.New("fixed TLS listener configuration required")
	}
	return value, nil
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
	backend, err := placement.Open(placement.Config{
		Harness: fixed.Harness, Kube: kube, RESTConfig: restConfig, Namespace: fixed.Namespace,
		TemplateName: fixed.TemplateName, Principal: fixed.Principal,
		Directory: fixed.Directory, Name: fixed.PlacementName,
		InstructionDigest: fixed.InstructionDigest, Allowances: fixed.Allowances,
		MaxDuration: duration, MaxCheckpointBytes: fixed.MaxCheckpointBytes,
	})
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	handler, err := profile.NewHandler(profile.Config{
		Harness: fixed.Harness, Principal: fixed.Principal, TokenFile: fixed.TokenFile, Placement: fixed.PlacementName,
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
		homeToken, readErr := os.ReadFile(fixed.TokenFile)
		if readErr != nil {
			return readErr
		}
		workHandler, handlerErr := work.NewHandler(owner,
			strings.TrimSpace(string(taskToken)), strings.TrimSpace(string(homeToken)))
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
