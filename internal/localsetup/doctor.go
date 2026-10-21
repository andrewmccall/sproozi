package localsetup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// Check is one diagnostic result. Component separates Docker, image,
// network, TLS, provider, service and state failures.
type Check struct {
	Component string `json:"component"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	Next      string `json:"next,omitempty"`
}

// Report is the doctor's complete result.
type Report struct {
	Root   string  `json:"root"`
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func (r *Report) add(component string, err error, detail string) {
	c := Check{Component: component, OK: err == nil, Detail: detail}
	if err != nil {
		c.Detail = err.Error()
		var f *Failure
		if errors.As(err, &f) {
			c.Detail, c.Next = f.Err.Error(), f.Next
		}
	}
	r.Checks = append(r.Checks, c)
}

// Doctor checks the installation without changing installation-owned state.
// The restricted-path probe starts and removes temporary probe containers.
// Provider validation lists models only and is opt-in.
func Doctor(ctx context.Context, l Layout, checkProvider bool) (r Report) {
	r = Report{Root: l.Root}
	defer func() {
		r.OK = true
		for _, c := range r.Checks {
			r.OK = r.OK && c.OK
		}
	}()
	for _, dir := range []string{l.Root, l.Credentials(), l.PKI(), l.State(), l.DockerCLI(), l.Logs(), l.Profile()} {
		if err := privatefs.Check(dir, true); err != nil {
			r.add("permissions", fail("permissions", err, "restore owner-only mode 0700 on "+dir), "")
			return r
		}
	}
	r.add("permissions", nil, "private directories are owner-only")
	inst, err := LoadInstallation(l)
	if err != nil || inst.Image == nil || inst.Network == nil || inst.Endpoint == nil || len(inst.Harnesses) == 0 {
		r.add("installation", fail("installation", fmt.Errorf("incomplete installation journal: %v", err), "run sproozi init"), "")
		return r
	}
	config, err := home.ReadConfig(l.Config())
	r.add("config", err, "configuration "+l.Config()+" is valid")
	if err != nil {
		return r
	}
	checkProfile(&r, config.Local.ProfileDirectory)
	docker, err := DiscoverDocker(ctx, l.DockerCLI())
	r.add("docker", err, "engine at "+docker.Socket+" runs Linux containers")
	if err != nil {
		return r
	}
	err = VerifyImage(ctx, docker, *inst.Image)
	if err == nil {
		err = verifyContract(ctx, docker, inst.Image)
	}
	r.add("image", err, fmt.Sprintf("%s provides helper contract %s", inst.Image.ID, helperContract))
	network, err := inspectNetwork(ctx, docker, inst.Network.Name, inst.ID)
	r.add("network", err, fmt.Sprintf("%s (%s) is owned by this installation", network.Name, network.Subnet))
	for _, h := range inst.Harnesses {
		provider := ProviderFor(h.Harness)
		_, err = privatefs.ReadFile(h.Credential, 16<<10)
		if err != nil {
			err = fail("credential", err, "sproozi credentials add "+provider.Name)
		}
		r.add("credential", err, fmt.Sprintf("%s credential for %s is stored privately", provider.Name, h.Harness))
		if err == nil && checkProvider {
			r.add("provider", CheckCredential(ctx, h.Harness, h.ProviderURL, h.ProviderCAFile, h.Credential), provider.Name+" accepted the credential")
		}
	}
	store, err := pki.Open(l.PKI(), pki.DefaultPolicy, nil)
	var trust []byte
	if err == nil {
		var status CertificateStatus
		status, err = Certificates(store)
		if err == nil {
			trust, err = privatefs.ReadFile(store.TrustPath(), 64<<10)
			r.add("tls", err, fmt.Sprintf("leaf expires %s, renews after %s; CA %d expires %s", status.LeafNotAfter.Format(time.RFC3339),
				status.RenewAt.Format(time.RFC3339), status.Serving, status.CANotAfter.Format(time.RFC3339)))
		}
	}
	if err != nil {
		r.add("tls", fail("tls", err, "run sproozi certificates status"), "")
		return r
	}
	service := ServiceState(ctx, l, config)
	if !service.Running {
		r.add("service", fail("service", errors.New("the service is not running"), "sproozi service start"), "")
		return r
	}
	r.add("service", nil, fmt.Sprintf("running with %d active session(s); log %s", len(service.Active), service.Log))
	if ip := net.ParseIP(inst.Endpoint.ListenHost); ip != nil && ip.IsUnspecified() {
		r.add("network", nil, fmt.Sprintf("warning: the gateway listens on every interface because no narrower address "+
			"reached workers; allow its ports only from the Docker network in the host firewall (ports %v)", ports(inst)))
	}
	for _, h := range inst.Harnesses {
		report, err := ProbePath(ctx, docker, inst.ID, *inst.Image, network, inst.Endpoint.GatewayIPv4, h.Port, trust)
		denied := 0
		for _, d := range report.Denied {
			if !d.Reached {
				denied++
			}
		}
		r.add("path", err, fmt.Sprintf("%s worker verified gateway TLS at %s and %d forbidden paths were denied", h.Harness, report.Gateway, denied))
	}
	return r
}

// CertificateStatus summarises the local CA and serving leaf.
type CertificateStatus struct {
	Serving      int           `json:"servingGeneration"`
	Trusted      []int         `json:"trustedGenerations"`
	Fingerprint  string        `json:"issuerFingerprint"`
	CANotAfter   time.Time     `json:"caNotAfter"`
	RotateAt     time.Time     `json:"rotateAt"`
	LeafNotAfter time.Time     `json:"leafNotAfter"`
	RenewAt      time.Time     `json:"renewAt"`
	Names        pki.Names     `json:"names"`
	Rotation     *pki.Rotation `json:"rotation,omitempty"`
	Waiting      []string      `json:"waitingFor,omitempty"`
}

// Certificates reports the store's committed state.
func Certificates(store *pki.Store) (CertificateStatus, error) {
	cert, _, st, err := store.Serving()
	if err != nil {
		return CertificateStatus{}, err
	}
	s := CertificateStatus{Serving: st.Serving, Trusted: st.Trusted, LeafNotAfter: cert.Leaf.NotAfter,
		RenewAt: store.Policy().RenewAt(cert.Leaf), Names: st.Names, Rotation: st.Rotation}
	for _, g := range st.Generations {
		if g.ID == st.Serving {
			s.Fingerprint, s.CANotAfter, s.RotateAt = g.Fingerprint, g.NotAfter, g.NotAfter.Add(-store.Policy().RotateBefore)
		}
	}
	if st.Rotation != nil {
		lacking, err := store.Lacking(st.Rotation.To)
		if err != nil {
			return s, err
		}
		for _, c := range lacking {
			s.Waiting = append(s.Waiting, c.ID)
		}
	}
	return s, nil
}

// PrepareImage resolves an image and makes it the installation's image when
// no local placement state depends on the current one.
func PrepareImage(ctx context.Context, l Layout, request ImageRequest) (Image, bool, error) {
	inst, err := LoadInstallation(l)
	if err != nil {
		return Image{}, false, err
	}
	docker, err := DiscoverDocker(ctx, l.DockerCLI())
	if err != nil {
		return Image{}, false, err
	}
	image, err := ResolveImage(ctx, docker, inst.ID, request)
	if err != nil {
		return Image{}, false, err
	}
	if inst.Image != nil && inst.Image.ID == image.ID {
		return image, true, nil
	}
	if placementExists(l) {
		return image, false, nil
	}
	unlock, err := l.lock()
	if err != nil {
		return image, false, err
	}
	defer unlock()
	inst.Image = &image
	if err = l.save(inst); err != nil {
		return image, false, err
	}
	if _, err = privatefs.ReadFile(l.Config(), 256<<10); err == nil {
		c, err := home.ReadConfig(l.Config())
		if err != nil {
			return image, false, err
		}
		c.Local.NativeImageID, c.Local.HelperImageID, c.Local.AnchorImageID = image.ID, image.ID, image.ID
		if err = writeConfig(l, c, true); err != nil {
			return image, false, err
		}
	}
	return image, true, nil
}

// PKIStore opens the installation's certificate store.
func PKIStore(l Layout) (*pki.Store, error) {
	if _, err := privatefs.ReadFile(filepath.Join(l.PKI(), "state.json"), 64<<10); err != nil {
		return nil, fmt.Errorf("no local certificate authority in %s: %w", l.PKI(), err)
	}
	return pki.Open(l.PKI(), pki.DefaultPolicy, nil)
}

func ports(inst Installation) []uint16 {
	out := make([]uint16, 0, len(inst.Harnesses))
	for _, h := range inst.Harnesses {
		out = append(out, h.Port)
	}
	return out
}

// checkProfile reports the profile every launch sends. Each launch archives
// it, so it warns before the archive outgrows its limit.
func checkProfile(r *Report, folder string) {
	profile, err := agentprofile.LoadDirectory(folder)
	if err != nil {
		r.add("profile", fail("profile", err, "fix "+folder+"; see the profile reference"), "")
		return
	}
	archive, _, err := profile.Archive()
	if err != nil {
		r.add("profile", fail("profile", fmt.Errorf("profile %s cannot be sent to a session: %w", folder, err),
			"remove skill files from "+folder), "")
		return
	}
	names := make([]string, 0, len(profile.Skills))
	for _, s := range profile.Skills {
		names = append(names, s.Name)
	}
	detail := fmt.Sprintf("%s: %d skills [%s], archive %d of %d bytes", folder, len(names), strings.Join(names, ", "),
		len(archive), agentprofile.MaxArchiveBytes)
	if len(archive) > agentprofile.MaxArchiveBytes*3/4 {
		detail = "warning: " + detail + "; near the profile size limit"
	}
	r.add("profile", nil, detail)
	if ignored, err := agentprofile.Ignored(folder); err == nil && len(ignored) > 0 {
		r.add("profile", nil, "ignored because no harness reads them: "+strings.Join(ignored, ", "))
	}
}
