// Package home composes the trusted local owner, fixed placements and home journal.
// Its private Unix socket is an administrator control channel, never mounted in workers.
package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

type Config struct {
	Directory        string                          `json:"directory"`
	DockerBinary     string                          `json:"dockerBinary"`
	DockerSocket     string                          `json:"dockerSocket"`
	DockerConfig     string                          `json:"dockerConfig"`
	Local            runtimedocker.PlacementConfig   `json:"local"`
	Allowances       map[string]session.Allowance    `json:"allowances"`
	MaxDuration      string                          `json:"maxDuration"`
	Model            string                          `json:"model"`
	NativeOptions    []string                        `json:"nativeOptions,omitempty"`
	Gateway          GatewayConfig                   `json:"gateway"`
	Remote           map[string]profile.ClientConfig `json:"remote,omitempty"`
	WorkSources      map[string]profile.ClientConfig `json:"workSources,omitempty"`
	ProjectApprovals []ProjectApproval               `json:"projectApprovals,omitempty"`
	// Harnesses adds further native harnesses to the home. Each is its own local
	// placement with its own gateway port, grants and provider credential.
	Harnesses []LocalHarness `json:"harnesses,omitempty"`
}

// LocalHarness is another native harness beside the primary local placement.
// It shares that placement's images, network, gateway address, instructions,
// limits, allowances and certificate source. A session keeps the harness it
// started with; its worker can reach only its own harness's gateway port.
type LocalHarness struct {
	Harness           harness.NativeKind `json:"harness"`
	Name              string             `json:"name"`
	GatewayPort       uint16             `json:"gatewayPort"`
	Model             string             `json:"model"`
	NativeOptions     []string           `json:"nativeOptions,omitempty"`
	ProviderURL       string             `json:"providerURL"`
	ProviderTokenFile string             `json:"providerTokenFile"`
	ProviderCAFile    string             `json:"providerCAFile,omitempty"`
	PricingFile       string             `json:"pricingFile,omitempty"`
}

// WorkConnections resolves remote work owners by name, for run, discovery,
// remote control and pull alike. A workSources entry wins; any other name uses
// its remote profile.
func (c Config) WorkConnections() map[string]profile.ClientConfig {
	connections := make(map[string]profile.ClientConfig, len(c.Remote)+len(c.WorkSources))
	maps.Copy(connections, c.Remote)
	maps.Copy(connections, c.WorkSources)
	return connections
}

// ProjectApproval is current local authority, qualified by authenticated source.
// It narrows home ceilings independently of imported project metadata.
type ProjectApproval struct {
	Source       string     `json:"source"`
	Project      project.ID `json:"project"`
	Capabilities []string   `json:"capabilities"`
}

type GatewayConfig struct {
	Listen          string `json:"listen"`
	CertificateFile string `json:"certificateFile,omitempty"`
	KeyFile         string `json:"keyFile,omitempty"`
	TrustBundleFile string `json:"trustBundleFile,omitempty"`
	// PKIDirectory selects a home-owned private CA store instead of the three
	// administrator files. The home renews its leaf and rotates the CA.
	PKIDirectory            string                        `json:"pkiDirectory,omitempty"`
	ProviderURL             string                        `json:"providerURL"`
	ProviderTokenFile       string                        `json:"providerTokenFile"`
	ProviderCAFile          string                        `json:"providerCAFile,omitempty"`
	PricingFile             string                        `json:"pricingFile,omitempty"`
	MCPAuthority            string                        `json:"mcpAuthority,omitempty"`
	MCPRegistryFile         string                        `json:"mcpRegistryFile,omitempty"`
	MCPCredentialsDirectory string                        `json:"mcpCredentialsDirectory,omitempty"`
	MCPPolicy               map[string]api.MCPServerScope `json:"mcpPolicy,omitempty"`
	// MaxModelCallSeconds is the longest one model call may stream, 1 to
	// 3600. Omitted selects the gateway default of 1200 seconds; a pointer
	// tells omission apart from an explicit 0, which is refused.
	MaxModelCallSeconds *CallSeconds      `json:"maxModelCallSeconds,omitempty"`
	GitHub              *GitHubConfig     `json:"github,omitempty"`
	Kubernetes          *KubernetesConfig `json:"kubernetes,omitempty"`
}

// Credentials are private gateway-owned files, never worker environment values.
type GitHubConfig struct {
	AppID          int64                      `json:"appID"`
	InstallationID int64                      `json:"installationID"`
	PrivateKeyFile string                     `json:"privateKeyFile"`
	Scope          api.GitHubPullRequestScope `json:"scope"`
}

// KubernetesConfig selects one exact API endpoint and its restricted credential.
// It intentionally does not import kubeconfig contexts, auth plugins or exec commands.
type KubernetesConfig struct {
	UpstreamURL string                  `json:"upstreamURL"`
	TokenFile   string                  `json:"tokenFile"`
	CAFile      string                  `json:"caFile"`
	Scope       api.KubernetesReadScope `json:"scope"`
}

func readPrivate(path string, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("home: absolute private file required")
	}
	return privatefs.ReadFile(path, max)
}

// readSetting reads the file a configuration key names. A file that fails the
// private-file check is reported by its key; the error unwraps to the
// privatefs error so the CLI can add the platform's fix.
func readSetting(key, path string, max int64) ([]byte, error) {
	data, err := readPrivate(path, max)
	var unsafe *privatefs.UnsafePathError
	if errors.As(err, &unsafe) {
		return nil, &settingFileError{key: key, path: path, err: err}
	}
	return data, err
}

// settingFileError explains why even a public certificate must be private:
// home applies one owner-only check to every file it is configured with.
type settingFileError struct {
	key, path string
	err       error
}

func (e *settingFileError) Error() string {
	return fmt.Sprintf("home: %s %s failed the private-file check; home applies one owner-only check "+
		"(yours, not a link, no access for any other account) to every file it reads, including public certificates", e.key, e.path)
}
func (e *settingFileError) Unwrap() error { return e.err }

func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	end := d.InputOffset()
	if d.Decode(new(any)) != io.EOF {
		return &trailingJSONError{offset: end}
	}
	return nil
}

// trailingJSONError reports content after the one JSON value a message or file holds.
type trailingJSONError struct{ offset int64 }

func (e *trailingJSONError) Error() string { return "home: trailing JSON" }

// InvalidConfigError reports a configuration file that was read but holds
// malformed JSON or a setting that fails validation. Its message names the key.
type InvalidConfigError struct{ Err error }

func (e *InvalidConfigError) Error() string { return e.Err.Error() }
func (e *InvalidConfigError) Unwrap() error { return e.Err }

func ReadConfig(path string) (Config, error) {
	b, err := readPrivate(path, 256<<10)
	if err != nil {
		return Config{}, err
	}
	c, err := parseConfig(b)
	if err != nil {
		return c, &InvalidConfigError{Err: err}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	c.Local.ProfileDirectory = filepath.Join(filepath.Dir(absolute), ProfileFolder)
	return c, nil
}

func parseConfig(b []byte) (Config, error) {
	var c Config
	if err := decode(b, &c); err != nil {
		var trailing *trailingJSONError
		if errors.As(err, &trailing) {
			return c, fmt.Errorf("unexpected content after the closing \"}\" that ends at byte %d; "+
				"the file must hold exactly one JSON object", trailing.offset)
		}
		return c, unknownFieldKey(b, typeErrorKey(b, err))
	}
	if err := requiredSettings(c); err != nil {
		return c, err
	}
	duration, err := time.ParseDuration(c.MaxDuration)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return c, errors.New("maxDuration: positive duration of at most 24h, such as \"30m\", required")
	}
	if err = validateConnections(c); err != nil {
		return c, err
	}
	if err = validateProjectApprovals(c); err != nil {
		return c, err
	}
	return c, nil
}

// ProfileFolder is the user's profile, beside the configuration file. Each
// Docker launch sends it as it is then.
const ProfileFolder = "profile"

// requiredSettings names, by key, each setting every command needs. Settings
// only serve needs, such as model, are in ServeProblems.
func requiredSettings(c Config) error {
	var problems []string
	if !filepath.IsAbs(c.Directory) {
		problems = append(problems, "directory: absolute path required")
	}
	if c.Local.Name == "" {
		problems = append(problems, "local.name: required; "+executorNameRule)
	} else if problem := nameProblem(c.Local.Name); problem != "" {
		problems = append(problems, "local.name: "+problem)
	}
	if len(c.Allowances) == 0 {
		problems = append(problems, "allowances: at least one capability ceiling required")
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// connectionGroup is one configuration map of remote work connections.
type connectionGroup struct {
	key     string
	entries map[string]profile.ClientConfig
}

func (c Config) connectionGroups() []connectionGroup {
	return []connectionGroup{{"remote", c.Remote}, {"workSources", c.WorkSources}}
}

// ReservedExecutor is the executor name that addresses home itself.
const ReservedExecutor = "docker"

// ReservedHome names the home daemon in commands and output, so no executor may take it.
const ReservedHome = "home"

// MaxCheckpointBytes is the largest checkpoint home's Docker placement
// captures. Remote entries must match local.maxCheckpointBytes, so it bounds them too.
const MaxCheckpointBytes = maxCapture

// executorName is a DNS label: executor names appear in commands, output and URLs.
var executorName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

const executorNameRule = "use 1 to 63 lowercase letters, digits and hyphens, starting and ending with a letter or digit"

// ReservedNames explains the names that address home, in every message about them.
const ReservedNames = `docker is home's executor name, home names the home daemon, ` +
	`and local.name names home's local placement for "move --to"`

// ExecutorNameProblem explains why name cannot name a remote executor or
// home's local placement, or returns "" when it can.
func ExecutorNameProblem(name string) string {
	switch {
	case name == ReservedExecutor || name == ReservedHome:
		return fmt.Sprintf("%q is reserved: %s", name, ReservedNames)
	case !executorName.MatchString(name):
		return fmt.Sprintf("%q is not a valid name; %s", name, executorNameRule)
	}
	return ""
}

// nameProblem explains a configured name that cannot be used, with the fix.
func nameProblem(name string) string {
	problem := ExecutorNameProblem(name)
	if name == ReservedExecutor || name == ReservedHome {
		problem += "; choose another name"
	}
	return problem
}

// validateConnections checks each remote and workSources entry by key, and
// rejects names that would shadow home or its local placement.
func validateConnections(c Config) error {
	var problems []string
	for _, group := range c.connectionGroups() {
		for _, name := range slices.Sorted(maps.Keys(group.entries)) {
			key := group.key + "." + name
			if problem := nameProblem(name); problem != "" {
				problems = append(problems, key+": "+problem)
				continue
			}
			if name == c.Local.Name {
				problems = append(problems, fmt.Sprintf("%s: %q is local.name, home's local placement; "+
					"choose another name or change local.name", key, name))
				continue
			}
			entry := group.entries[name]
			// Home checks the checkpoint limit itself: the profile protocol allows
			// more than home's placement can capture.
			checked := entry
			checked.MaxCheckpointBytes = 1
			if err := checked.Validate(); err != nil {
				for problem := range strings.SplitSeq(err.Error(), "\n") {
					problems = append(problems, key+"."+problem)
				}
			}
			if problem := checkpointLimitProblem(entry.MaxCheckpointBytes, c.Local.MaxCheckpointBytes); problem != "" {
				problems = append(problems, key+".maxCheckpointBytes: "+problem)
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// checkpointLimitProblem checks a remote entry's checkpoint limit against the
// one range home supports and, once it is set, local.maxCheckpointBytes.
func checkpointLimitProblem(remote, local int64) string {
	const rule = "1 byte to 64 MiB, equal to local.maxCheckpointBytes"
	switch {
	case remote < 1 || remote > MaxCheckpointBytes:
		return fmt.Sprintf("%d is out of range; %s, required", remote, rule)
	case local != 0 && remote != local:
		return fmt.Sprintf("%d differs from local.maxCheckpointBytes (%d); %s, required", remote, local, rule)
	}
	return ""
}

// typeErrorKey restates a JSON type error by configuration key, such as
// local.gatewayPort or remote.NAME.maxCheckpointBytes, instead of Go names.
func typeErrorKey(data []byte, err error) error {
	var typed *json.UnmarshalTypeError
	if !errors.As(err, &typed) || typed.Field == "" {
		return err
	}
	segments := strings.Split(typed.Field, ".")
	for i, segment := range segments {
		// Resource limit fields keep the capitalised spelling the reference uses.
		if segment != "" && (i == 0 || !strings.HasSuffix(segments[i-1], "Limits")) {
			segments[i] = strings.ToLower(segment[:1]) + segment[1:]
		}
	}
	// Go omits map keys from the path, so find the entry that fails alone.
	if named := map[string]bool{"remote": true, "workSources": true, "allowances": true}; named[segments[0]] {
		var top map[string]json.RawMessage
		var group map[string]json.RawMessage
		if json.Unmarshal(data, &top) == nil && json.Unmarshal(top[segments[0]], &group) == nil {
			for _, name := range slices.Sorted(maps.Keys(group)) {
				var entry any = new(profile.ClientConfig)
				if segments[0] == "allowances" {
					entry = new(session.Allowance)
				}
				if json.Unmarshal(group[name], entry) != nil {
					segments = slices.Insert(segments, 1, name)
					break
				}
			}
		}
	}
	return fmt.Errorf("%s: %s is not %s", strings.Join(segments, "."), typed.Value, jsonKind(typed.Type))
}

// unknownFieldKey restates a JSON unknown field error with the configuration
// key that holds the field, such as remote.NAME, which Go leaves out.
func unknownFieldKey(data []byte, err error) error {
	if !strings.HasPrefix(err.Error(), "json: unknown field ") {
		return err
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return err
	}
	path, field, ok := findUnknownField(v, reflect.TypeFor[Config](), nil)
	if !ok {
		return err
	}
	if len(path) == 0 {
		return fmt.Errorf("unknown key %q", field)
	}
	return fmt.Errorf("%s: unknown key %q", strings.Join(path, "."), field)
}

var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

// findUnknownField walks decoded JSON alongside the Go type it decodes into and
// returns the first object key, in sorted order, that no field accepts.
func findUnknownField(v any, t reflect.Type, path []string) ([]string, string, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler) {
		return nil, "", false
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := v.(map[string]any)
		if !ok {
			return nil, "", false
		}
		fields := jsonFields(t)
		for _, key := range slices.Sorted(maps.Keys(object)) {
			field, ok := fields[key]
			if !ok {
				for name, f := range fields {
					if strings.EqualFold(name, key) {
						field, ok = f, true
						break
					}
				}
			}
			if !ok {
				return path, key, true
			}
			if p, f, found := findUnknownField(object[key], field, append(slices.Clip(path), key)); found {
				return p, f, true
			}
		}
	case reflect.Map:
		object, ok := v.(map[string]any)
		if !ok {
			return nil, "", false
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if p, f, found := findUnknownField(object[key], t.Elem(), append(slices.Clip(path), key)); found {
				return p, f, true
			}
		}
	case reflect.Slice, reflect.Array:
		items, _ := v.([]any)
		for i, item := range items {
			indexed := slices.Clone(path)
			if len(indexed) > 0 {
				indexed[len(indexed)-1] += fmt.Sprintf("[%d]", i)
			}
			if p, f, found := findUnknownField(item, t.Elem(), indexed); found {
				return p, f, true
			}
		}
	}
	return nil, "", false
}

// jsonFields maps each JSON name a struct accepts to its Go type, promoting
// the fields of untagged embedded structs as encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			embedded := f.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				for n, ft := range jsonFields(embedded) {
					if _, ok := fields[n]; !ok {
						fields[n] = ft
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}

// jsonKind describes the JSON value a Go type accepts.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		return fmt.Sprintf("a whole number from 0 to %d", uint64(math.MaxUint64)>>(64-t.Bits()))
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		if t == reflect.TypeFor[time.Duration]() {
			return "a whole number of nanoseconds"
		}
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

// CallSeconds is a whole number of seconds. Any JSON number with an integral
// value decodes, so 1000.0 is 1000; one too large for an int decodes as just
// out of range, so ServeProblems can name the range rather than the type.
type CallSeconds int

func (c *CallSeconds) UnmarshalJSON(data []byte) error {
	var number json.Number
	value, ok := new(big.Rat), false
	// json.Number also accepts a quoted number; the setting is a number.
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte(`"`)) && json.Unmarshal(data, &number) == nil {
		text := number.String()
		// An exponent too large to evaluate makes the value far out of range,
		// or, if negative, a fraction. A zero mantissa is zero either way.
		if mantissa, exponent, found := strings.Cut(strings.ToLower(text), "e"); found {
			scale, err := strconv.ParseInt(strings.TrimPrefix(exponent, "+"), 10, 64)
			zero := strings.Trim(mantissa, "-+0.") == ""
			switch {
			case zero:
				text = "0"
			case err != nil && !strings.HasPrefix(exponent, "-") || err == nil && scale > 400:
				*c = math.MaxInt32
				if strings.HasPrefix(mantissa, "-") {
					*c = -1
				}
				return nil
			case err != nil || scale < -400:
				text = "0.5" // a fraction too small to evaluate
			}
		}
		value, ok = value.SetString(text)
	}
	if !ok || !value.IsInt() {
		return fmt.Errorf("gateway.maxModelCallSeconds: %s is not a whole number of seconds", data)
	}
	// Clamp far outside the 1 to 3600 range without overflowing.
	whole := value.Num()
	switch {
	case whole.Cmp(big.NewInt(0)) < 0:
		*c = -1
	case whole.Cmp(big.NewInt(math.MaxInt32)) > 0:
		*c = math.MaxInt32
	default:
		*c = CallSeconds(whole.Int64())
	}
	return nil
}
