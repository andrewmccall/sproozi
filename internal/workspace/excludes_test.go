package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

const fixtureSecret = ".env"

// userHome points the real-user Git environment at a temporary home with no
// global config, so tests never read the developer's own Git settings.
func userHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	if err := os.Unsetenv("GIT_CONFIG_GLOBAL"); err != nil {
		t.Fatal(err)
	}
	return home
}

func restoredFiles(t *testing.T, data []byte) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), data, dst, testLimit); err != nil {
		t.Fatal(err)
	}
	return dst
}

func assertAbsent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Fatalf("%s reached the restored workspace: %v", name, err)
	}
}

// TestUserExcludesFileResolvesEveryUserScope checks that each place the
// user's Git reads core.excludesFile from resolves to that file.
func TestUserExcludesFileResolvesEveryUserScope(t *testing.T) {
	cases := map[string]func(t *testing.T, home, root string) string{
		"default global ignore": func(t *testing.T, home, _ string) string {
			writeFixture(t, home, ".config/git/ignore", []byte(fixtureSecret+"\n"), 0600)
			return filepath.Join(home, ".config/git/ignore")
		},
		"XDG global ignore": func(t *testing.T, home, _ string) string {
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
			writeFixture(t, home, "xdg/git/ignore", []byte(fixtureSecret+"\n"), 0600)
			return filepath.Join(home, "xdg/git/ignore")
		},
		"global core.excludesFile with tilde": func(t *testing.T, home, _ string) string {
			writeFixture(t, home, ".gitignore_global", []byte(fixtureSecret+"\n"), 0600)
			writeFixture(t, home, ".gitconfig", []byte("[core]\n\texcludesFile = ~/.gitignore_global\n"), 0600)
			return filepath.Join(home, ".gitignore_global")
		},
		"repository core.excludesFile relative to the work tree": func(t *testing.T, _, root string) string {
			writeFixture(t, root, "ignore-rules", []byte(fixtureSecret+"\n"), 0644)
			fixtureGit(t, root, "config", "core.excludesFile", "ignore-rules")
			return filepath.Join(root, "ignore-rules")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			home := userHome(t)
			root := newFixture(t)
			want := setup(t, home, root)
			got, err := userExcludesFile(t.Context(), root)
			if err != nil || got != want {
				t.Fatalf("excludes file = %q, %v; want %q", got, err, want)
			}
		})
	}
}

// TestCaptureLocalAppliesUserExcludesFile checks that a file ignored only by
// the user's excludes file stays out of a capture unless it is selected.
func TestCaptureLocalAppliesUserExcludesFile(t *testing.T) {
	home := userHome(t)
	root := newFixture(t)
	writeFixture(t, root, fixtureSecret, []byte("TOKEN=secret\n"), 0600)
	writeFixture(t, home, ".config/git/ignore", []byte(fixtureSecret+"\n"), 0600)

	data, err := CaptureLocal(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	dst := restoredFiles(t, data)
	assertAbsent(t, dst, fixtureSecret)
	assertFile(t, dst, "new.txt", []byte("unstaged new\n"))

	data, err = CaptureLocal(t.Context(), root, []string{fixtureSecret}, testLimit)
	if err != nil {
		t.Fatalf("explicit selection of a globally ignored file: %v", err)
	}
	assertFile(t, restoredFiles(t, data), fixtureSecret, []byte("TOKEN=secret\n"))
}

func TestCaptureLocalRepositoryExcludesFileOverridesGlobal(t *testing.T) {
	home := userHome(t)
	root := newFixture(t)
	writeFixture(t, root, fixtureSecret, []byte("TOKEN=secret\n"), 0600)
	writeFixture(t, home, ".config/git/ignore", []byte(fixtureSecret+"\n"), 0600)
	writeFixture(t, home, "empty-rules", nil, 0600)
	fixtureGit(t, root, "config", "core.excludesFile", filepath.Join(home, "empty-rules"))

	data, err := CaptureLocal(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	// Git uses the repository value instead of the default global file.
	assertFile(t, restoredFiles(t, data), fixtureSecret, []byte("TOKEN=secret\n"))
}

func TestCaptureLocalIgnoresGlobalIncludes(t *testing.T) {
	home := userHome(t)
	root := newFixture(t)
	writeFixture(t, root, fixtureSecret, []byte("TOKEN=secret\n"), 0600)
	writeFixture(t, home, ".config/git/ignore", []byte(fixtureSecret+"\n"), 0600)
	writeFixture(t, home, "included", []byte("[core]\n\texcludesFile = /nonexistent\n"), 0600)
	writeFixture(t, home, ".gitconfig", []byte("[include]\n\tpath = ~/included\n"), 0600)

	data, err := CaptureLocal(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, restoredFiles(t, data), fixtureSecret)
}

func TestCaptureLeavesUserExcludesToCaptureLocal(t *testing.T) {
	home := userHome(t)
	root := newFixture(t)
	writeFixture(t, root, fixtureSecret, []byte("TOKEN=secret\n"), 0600)
	writeFixture(t, home, ".config/git/ignore", []byte(fixtureSecret+"\n"), 0600)

	data, err := Capture(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	// Sandbox checkpoints run in a helper without the user's Git config.
	assertFile(t, restoredFiles(t, data), fixtureSecret, []byte("TOKEN=secret\n"))
}
