// Command tasks serves private, fixed-workflow orchestration over native MCP.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/tasks"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	data, err := os.ReadFile(os.Getenv("SPROOZI_TASK_CONFIG"))
	if err != nil {
		return fmt.Errorf("task configuration unavailable")
	}
	if len(data) > 64<<10 {
		return fmt.Errorf("task configuration too large")
	}
	var settings tasks.Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&settings); err != nil {
		return fmt.Errorf("invalid task configuration")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("invalid trailing task configuration")
	}
	token, err := os.ReadFile(os.Getenv("SPROOZI_TASK_TOKEN_FILE"))
	if err != nil {
		return fmt.Errorf("task token unavailable")
	}
	config, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	config.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	if err = api.AddToScheme(scheme); err != nil {
		return err
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	service, err := tasks.NewService(kube, settings)
	if err != nil {
		return err
	}
	handler, err := tasks.NewHandler(service, strings.TrimSpace(string(token)))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8443", Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 45 * time.Second,
		WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	log.Print("Task MCP listening on :8443")
	return server.ListenAndServeTLS(os.Getenv("SPROOZI_TASK_CERT_FILE"), os.Getenv("SPROOZI_TASK_KEY_FILE"))
}
