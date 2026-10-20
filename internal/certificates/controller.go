// Package certificates keeps a Kubernetes installation's gateway certificate
// authority without cert-manager. One controller owns three objects: a
// precreated authority Secret holding the CA store, the gateway's precreated
// serving Secret, and immutable trust ConfigMaps named by trust generation.
// It reuses the local certificate store, so renewal and rotation follow the
// same rules everywhere.
package certificates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Annotations on the authority Secret. RotationRequest is the operator's
// explicit instruction; the others report status.
const (
	RotationRequest  = "sproozi.com/rotation-request"
	TrustAnnotation  = "sproozi.com/trust-configmap"
	LeafNotAfter     = "sproozi.com/leaf-not-after"
	RenewAt          = "sproozi.com/renew-at"
	RotationStage    = "sproozi.com/rotation-stage"
	LastError        = "sproozi.com/last-error"
	ManagedByLabel   = "app.kubernetes.io/managed-by"
	managedByValue   = "sproozi-certificates"
	trustBundleKey   = "ca-bundle.pem"
	maxAuthorityKeys = 64
)

// Config names the objects and identities the controller owns.
type Config struct {
	Namespace       string
	AuthoritySecret string
	ServingSecret   string
	TrustNamespace  string
	TrustPrefix     string
	Names           pki.Names
	Policy          pki.Policy
	// Scratch is a private memory-backed directory for the store's files.
	Scratch string
	Now     func() time.Time
}

// Controller reconciles the certificate objects.
type Controller struct {
	Client kubernetes.Interface
	Config Config
}

// Status is one reconcile's result.
type Status struct {
	TrustConfigMap string
	LeafNotAfter   time.Time
	Rotation       *pki.Rotation
	Changed        bool
}

var storeKey = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// Reconcile applies any rotation request, ensures a current serving leaf,
// commits the authority Secret with optimistic concurrency, then publishes
// the serving Secret and the trust ConfigMap derived from committed state.
func (c *Controller) Reconcile(ctx context.Context) (Status, error) {
	cfg := c.Config
	secrets := c.Client.CoreV1().Secrets(cfg.Namespace)
	authority, err := secrets.Get(ctx, cfg.AuthoritySecret, metav1.GetOptions{})
	if err != nil {
		return Status{}, fmt.Errorf("read authority Secret %s/%s (install creates it): %w", cfg.Namespace, cfg.AuthoritySecret, err)
	}
	dir, err := privatefs.TempDir(cfg.Scratch, "store-")
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if len(authority.Data) > maxAuthorityKeys {
		return Status{}, errors.New("authority Secret has too many keys")
	}
	for key, value := range authority.Data {
		if !storeKey.MatchString(key) {
			return Status{}, fmt.Errorf("authority Secret has unexpected key %q", key)
		}
		if err = privatefs.Replace(dir, key, value); err != nil {
			return Status{}, err
		}
	}
	store, err := pki.Open(dir, cfg.Policy, cfg.Now)
	if err != nil {
		return Status{}, err
	}
	request := authority.Annotations[RotationRequest]
	requestErr := c.apply(store, request)
	if _, err = store.Ensure(cfg.Names); err != nil {
		return Status{}, err
	}
	data, err := readStore(dir)
	if err != nil {
		return Status{}, err
	}
	certificate, trust, st, err := store.Serving()
	if err != nil {
		return Status{}, err
	}
	status := Status{TrustConfigMap: TrustName(cfg.TrustPrefix, st.Trusted), LeafNotAfter: certificate.Leaf.NotAfter, Rotation: st.Rotation}
	annotations := maps.Clone(authority.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	delete(annotations, RotationRequest)
	delete(annotations, LastError)
	if requestErr != nil {
		annotations[LastError] = fmt.Sprintf("rotation request %q refused: %v", request, requestErr)
	}
	annotations[TrustAnnotation] = status.TrustConfigMap
	annotations[LeafNotAfter] = certificate.Leaf.NotAfter.UTC().Format(time.RFC3339)
	annotations[RenewAt] = cfg.Policy.RenewAt(certificate.Leaf).UTC().Format(time.RFC3339)
	annotations[RotationStage] = "none"
	if st.Rotation != nil {
		annotations[RotationStage] = fmt.Sprintf("%s %d to %d", st.Rotation.Stage, st.Rotation.From, st.Rotation.To)
	}
	if !maps.EqualFunc(authority.Data, data, bytes.Equal) || !maps.Equal(authority.Annotations, annotations) {
		authority.Data, authority.Annotations = data, annotations
		if _, err = secrets.Update(ctx, authority, metav1.UpdateOptions{}); err != nil {
			// A conflict means another writer committed first; the next
			// reconcile starts from its state. Nothing derived was published.
			return Status{}, fmt.Errorf("commit authority Secret: %w", err)
		}
		status.Changed = true
	}
	if err = c.publishServing(ctx, store, st); err != nil {
		return status, err
	}
	if err = c.publishTrust(ctx, status.TrustConfigMap, trust); err != nil {
		return status, err
	}
	return status, requestErr
}

func (c *Controller) apply(store *pki.Store, request string) error {
	if _, err := store.State(); errors.Is(err, pki.ErrNotInitialized) {
		if request != "" {
			return errors.New("no authority exists yet")
		}
		return nil
	}
	switch request {
	case "":
		return nil
	case "renew":
		return store.Renew()
	case "prepare":
		return store.PrepareRotation()
	case "switch":
		// The operator asserts every runtime now references the combined
		// trust and every older Pod has stopped; see the rotation guide.
		return store.SwitchRotation(true)
	case "retire":
		return store.RetireRotation(true)
	case "rollback":
		return store.RollbackRotation()
	case "replace":
		return store.ReplaceCA()
	}
	return fmt.Errorf("unknown request; use renew, prepare, switch, retire, rollback or replace")
}

// publishServing writes the leaf and key the gateway mounts. The gateway
// reloads the projected files without restarting.
func (c *Controller) publishServing(ctx context.Context, store *pki.Store, st pki.State) error {
	pair, err := readLeaf(store.Dir(), st.Leaf)
	if err != nil {
		return err
	}
	secrets := c.Client.CoreV1().Secrets(c.Config.Namespace)
	serving, err := secrets.Get(ctx, c.Config.ServingSecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read serving Secret %s/%s (install creates it): %w", c.Config.Namespace, c.Config.ServingSecret, err)
	}
	want := map[string][]byte{corev1.TLSCertKey: pair.Certificate, corev1.TLSPrivateKeyKey: pair.Key}
	if maps.EqualFunc(serving.Data, want, bytes.Equal) {
		return nil
	}
	serving.Data = want
	_, err = secrets.Update(ctx, serving, metav1.UpdateOptions{})
	return err
}

// publishTrust creates the immutable certificate-only bundle for the current
// trust generations. Existing objects are never patched.
func (c *Controller) publishTrust(ctx context.Context, name string, trust []byte) error {
	configMaps := c.Client.CoreV1().ConfigMaps(c.Config.TrustNamespace)
	existing, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if existing.Data[trustBundleKey] != string(trust) {
			return fmt.Errorf("trust ConfigMap %s exists with different certificates; delete it only if no runtime references it", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	immutable := true
	_, err = configMaps.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Config.TrustNamespace, Labels: map[string]string{ManagedByLabel: managedByValue}},
		Data:       map[string]string{trustBundleKey: string(trust)}, Immutable: &immutable}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// TrustName names a trust ConfigMap by its generations. The first
// installation's bundle keeps the plain prefix so the quickstart can name it.
func TrustName(prefix string, generations []int) string {
	if len(generations) == 1 && generations[0] == 1 {
		return prefix
	}
	parts := make([]string, len(generations))
	for i, g := range generations {
		parts[i] = strconv.Itoa(g)
	}
	return prefix + "-" + strings.Join(parts, "-")
}

func readStore(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "owner.lock" || !storeKey.MatchString(name) {
			continue
		}
		value, err := privatefs.ReadFile(filepath.Join(dir, name), 64<<10)
		if err != nil {
			return nil, err
		}
		data[name] = value
	}
	return data, nil
}

func readLeaf(dir, name string) (pki.Pair, error) {
	cert, err := privatefs.ReadFile(filepath.Join(dir, name+".pem"), 64<<10)
	if err != nil {
		return pki.Pair{}, err
	}
	key, err := privatefs.ReadFile(filepath.Join(dir, name+".key"), 64<<10)
	return pki.Pair{Certificate: cert, Key: key}, err
}
