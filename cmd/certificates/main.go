// Command certificates maintains a Kubernetes installation's self-managed
// gateway certificate authority. It owns only its precreated Secrets and the
// immutable trust ConfigMaps it creates; it never reads other Secrets.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/certificates"
	"github.com/andrewmccall/sproozi/internal/pki"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type names []string

func (n *names) String() string     { return strings.Join(*n, ",") }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

func main() {
	var dns names
	cfg := certificates.Config{Policy: pki.DefaultPolicy}
	flag.StringVar(&cfg.Namespace, "namespace", "sproozi-system", "namespace of the authority and serving Secrets")
	flag.StringVar(&cfg.AuthoritySecret, "authority-secret", "sproozi-certificate-authority",
		"precreated Secret holding the CA store")
	flag.StringVar(&cfg.ServingSecret, "serving-secret", "sproozi-gateway-tls", "precreated Secret the gateway mounts")
	flag.StringVar(&cfg.TrustNamespace, "trust-namespace", "sproozi-agents", "namespace for worker trust ConfigMaps")
	flag.StringVar(&cfg.TrustPrefix, "trust-prefix", "sproozi-sandbox-trust", "trust ConfigMap name prefix")
	flag.StringVar(&cfg.Scratch, "scratch", "/var/run/sproozi-certificates", "private memory-backed directory")
	flag.Var(&dns, "dns", "DNS name the gateway certificate must cover; repeatable")
	interval := flag.Duration("interval", 5*time.Minute, "reconcile interval")
	flag.Parse()
	if len(dns) == 0 {
		dns = names{"sproozi-gateway.sproozi-system.svc", "sproozi-gateway.sproozi-system.svc.cluster.local",
			"kubernetes.default.svc"}
	}
	cfg.Names = pki.Names{DNS: dns}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &certificates.Controller{Client: client, Config: cfg}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		status, err := c.Reconcile(ctx)
		if err != nil {
			log.Printf("Failed to reconcile gateway certificates err=%q", err)
		} else if status.Changed {
			log.Printf("Updated gateway certificates trustConfigMap=%s leafNotAfter=%s", status.TrustConfigMap,
				status.LeafNotAfter.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
