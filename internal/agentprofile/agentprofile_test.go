package agentprofile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
)

const reviewSkill = "skills/review/SKILL.md"

func skill(name, description string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + description + "\n---\nBody\n")
}

func TestLoadReadsInstructionsAndSkills(t *testing.T) {
	fsys := fstest.MapFS{
		agentprofile.InstructionsFile:       {Data: []byte("Prefer small commits.\n")},
		"README.txt":                        {Data: []byte("ignored")},
		reviewSkill:                         {Data: skill("review", "Review code")},
		"skills/review/scripts/check.sh":    {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"skills/home-ops/SKILL.md":          {Data: skill("home-ops", "Operate the home lab")},
		"skills/home-ops/references/net.md": {Data: []byte("vlan 10")},
	}
	config, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if config.Instructions != "Prefer small commits.\n" {
		t.Fatalf("instructions = %q", config.Instructions)
	}
	if len(config.Skills) != 2 || config.Skills[0].Name != "home-ops" || config.Skills[1].Name != "review" {
		t.Fatalf("skills = %+v", config.Skills)
	}
	review := config.Skills[1]
	if len(review.Files) != 2 || review.Files[0].Path != "SKILL.md" || review.Files[1].Path != "scripts/check.sh" ||
		!review.Files[1].Executable || review.Files[0].Executable {
		t.Fatalf("review files = %+v", review.Files)
	}
}

func TestLoadAcceptsAnEmptyFolder(t *testing.T) {
	config, err := agentprofile.Load(fstest.MapFS{})
	if err != nil || config.Instructions != "" || len(config.Skills) != 0 {
		t.Fatalf("config = %+v, %v", config, err)
	}
}

func TestLoadRejectsUnsafeOrReservedContent(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"reserved skill":      {"skills/sproozi-access/SKILL.md": {Data: skill("sproozi-access", "x")}},
		"hidden skill":        {"skills/.system/SKILL.md": {Data: skill("system", "x")}},
		"invalid skill name":  {"skills/Review_Code/SKILL.md": {Data: skill("Review_Code", "x")}},
		"missing SKILL.md":    {"skills/review/notes.md": {Data: []byte("x")}},
		"file in skills":      {"skills/notes.md": {Data: []byte("x")}},
		"symlink":             {reviewSkill: {Data: skill("review", "x")}, "skills/review/link": {Data: []byte("/etc/passwd"), Mode: os.ModeSymlink}},
		"binary instructions": {agentprofile.InstructionsFile: {Data: []byte{0xff, 0xfe}}},
		"large instructions":  {agentprofile.InstructionsFile: {Data: []byte(strings.Repeat("a", agentprofile.MaxInstructionsBytes+1))}},
		"instructions dir":    {"AGENTS.md/x": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := agentprofile.Load(fsys); err == nil {
				t.Fatal("Load accepted unsafe profile")
			}
		})
	}
}

func TestLoadEnforcesFileAndByteLimits(t *testing.T) {
	many := fstest.MapFS{reviewSkill: {Data: skill("review", "x")}}
	for i := range agentprofile.MaxFiles {
		many[filepath.Join("skills/review/refs", strings.Repeat("f", 1+i%50)+string(rune('a'+i/50%26))+".md")] = &fstest.MapFile{Data: []byte("x")}
	}
	if _, err := agentprofile.Load(many); err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("file limit error = %v", err)
	}
	large := fstest.MapFS{
		reviewSkill:              {Data: skill("review", "x")},
		"skills/review/data.bin": {Data: make([]byte, agentprofile.MaxBytes+1)},
	}
	if _, err := agentprofile.Load(large); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("byte limit error = %v", err)
	}
}

func TestArchiveIsDeterministicAndRoundTrips(t *testing.T) {
	fsys := fstest.MapFS{
		agentprofile.InstructionsFile:    {Data: []byte("Be brief.\n"), Mode: 0o600},
		reviewSkill:                      {Data: skill("review", "Review code")},
		"skills/review/scripts/check.sh": {Data: []byte("#!/bin/sh\n"), Mode: 0o700},
	}
	config, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	first, digest, err := config.Archive()
	if err != nil {
		t.Fatal(err)
	}
	// Different source modes for non-executable files must not change identity.
	fsys[reviewSkill].Mode = 0o600
	again, err := agentprofile.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := again.Archive()
	if err != nil || string(first) != string(second) || digest != secondDigest {
		t.Fatalf("archive not deterministic: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest = %q", digest)
	}
	restored, err := agentprofile.FromArchive(first)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Instructions != config.Instructions || len(restored.Skills) != 1 ||
		len(restored.Skills[0].Files) != 2 || !restored.Skills[0].Files[1].Executable {
		t.Fatalf("restored = %+v", restored)
	}
	if _, err := agentprofile.FromArchive(append([]byte{}, first[:len(first)/2]...)); err == nil {
		t.Fatal("truncated archive accepted")
	}
}

func TestArchiveRejectsConfigurationOverTheConfigMapLimit(t *testing.T) {
	// Random-looking content defeats compression, so the archive exceeds the limit.
	data := make([]byte, agentprofile.MaxArchiveBytes+1024)
	state := uint32(1)
	for i := range data {
		state = state*1664525 + 1013904223
		data[i] = byte(state >> 24)
	}
	config, err := agentprofile.Load(fstest.MapFS{
		reviewSkill:              {Data: skill("review", "x")},
		"skills/review/data.bin": {Data: data},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.Archive(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("archive limit error = %v", err)
	}
}

func TestLoadDirectoryRejectsSymlinkedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "review", "SKILL.md"), skill("review", "x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := agentprofile.LoadDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, agentprofile.InstructionsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := agentprofile.LoadDirectory(dir); err == nil {
		t.Fatal("LoadDirectory followed a symlink")
	}
	missing, err := agentprofile.LoadDirectory(filepath.Join(dir, "absent"))
	if err != nil || missing.Instructions != "" || len(missing.Skills) != 0 {
		t.Fatalf("absent folder = %+v, %v", missing, err)
	}
}
