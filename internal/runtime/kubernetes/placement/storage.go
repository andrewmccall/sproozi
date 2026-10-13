package placement

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"

	registry "github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const recordSuffix = ".json"

type retained struct {
	Initial         *session.Launch    `json:"initial,omitempty"`
	WorkspaceDigest string             `json:"workspaceDigest,omitempty"`
	Launch          session.Launch     `json:"launch"`
	Reference       registry.Reference `json:"reference"`
	OutputDigest    string             `json:"outputDigest,omitempty"`
	NativeQuiet     bool               `json:"nativeQuiet,omitempty"`
	WriterFenced    bool               `json:"writerFenced,omitempty"`
}

func privatePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	if !ok || owner.Uid != uint32(os.Getuid()) || info.IsDir() != dir || info.Mode().Perm() != mode || info.Mode()&os.ModeSymlink != 0 || !dir && !info.Mode().IsRegular() {
		return errors.New("placement: private owned storage required")
	}
	return nil
}
func sameIntent(a, b session.Launch) bool {
	a.Deadline = a.Deadline.UTC()
	b.Deadline = b.Deadline.UTC()
	a.IncludeIgnored = append([]string(nil), a.IncludeIgnored...)
	b.IncludeIgnored = append([]string(nil), b.IncludeIgnored...)
	return reflect.DeepEqual(a, b)
}
func (p *Placement) persistIntent(l session.Launch, data []byte) (retained, error) {
	l.Deadline = l.Deadline.UTC()
	old, err := p.load(l)
	if err == nil {
		return old, nil
	}
	if !os.IsNotExist(err) {
		return retained{}, err
	}
	// One allocation per principal/session epoch prevents alternative allocation
	// IDs from replaying the same epoch through another deterministic Run name.
	epochPath := filepath.Join(p.config.Directory, "epoch-"+l.SessionID+"-"+strconv.FormatUint(l.Epoch, 10)+recordSuffix)
	var previous session.Launch
	if epochData, readErr := p.read(epochPath, 64<<10); readErr == nil {
		if decode(epochData, &previous) != nil || !sameIntent(l, previous) {
			return retained{}, &profile.Rejection{Code: profile.CodeConflict}
		}
	} else if !os.IsNotExist(readErr) {
		return retained{}, readErr
	} else {
		encoded, marshalErr := json.Marshal(l)
		if marshalErr != nil {
			return retained{}, marshalErr
		}
		if err = p.atomic(epochPath, encoded); err != nil {
			return retained{}, err
		}
	}
	if err = p.atomic(p.artifactPath(l.AllocationID, "input"), data); err != nil {
		return retained{}, err
	}
	r := retained{Launch: l}
	return r, p.save(r)
}
func (p *Placement) load(l session.Launch) (retained, error) {
	if !idValid(l.AllocationID) {
		return retained{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	data, err := p.read(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix), 64<<10)
	if err != nil {
		return retained{}, err
	}
	var r retained
	if err = decode(data, &r); err != nil {
		return retained{}, err
	}
	if !sameIntent(r.Launch, l) && (r.Initial == nil || !sameIntent(*r.Initial, l)) {
		return retained{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	return r, nil
}
func (p *Placement) save(r retained) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return p.atomic(filepath.Join(p.config.Directory, r.Launch.AllocationID+recordSuffix), data)
}
func (p *Placement) artifactPath(id, kind string) string {
	return filepath.Join(p.config.Directory, id+"-"+kind+".checkpoint")
}
func (p *Placement) readArtifact(id, kind string) ([]byte, error) {
	return p.read(p.artifactPath(id, kind), p.config.MaxCheckpointBytes)
}
func (p *Placement) read(path string, max int64) ([]byte, error) {
	if err := privatePath(path, false); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("placement: retained data exceeds limit")
	}
	return data, nil
}
func (p *Placement) atomic(path string, data []byte) error {
	f, err := os.CreateTemp(p.config.Directory, ".pending-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(p.config.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("placement: trailing JSON")
	}
	return nil
}
