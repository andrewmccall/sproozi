//go:build windows

package privatefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLongPrivatePaths(t *testing.T) {
	dir := testDirectory(t)
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("long", 8))
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := Lock(filepath.Join(dir, "owner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = UnlockClose(lock) }()
	if err = Replace(dir, "record.json", []byte(`{"value":"complete"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(filepath.Join(dir, "record.json"), 128)
	if err != nil || string(data) != `{"value":"complete"}` {
		t.Fatalf("long-path record: %q, %v", data, err)
	}
}

func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCreatedACLAndReplacement(t *testing.T) {
	dir := testDirectory(t)
	if err := Replace(dir, "credential", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "credential")} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !owner.Equals(user) {
			t.Fatalf("unexpected owner: %v %v", owner, err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("DACL allows parent inheritance: %v %v", control, err)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatalf("expected user and SYSTEM grants: %v %v", acl, err)
		}
		if err = Check(path, path == dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsRejectsBroadNullAndInheritedACLs(t *testing.T) {
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, sddl string }{
		{"everyone", "D:P(A;;FA;;;" + user.String() + ")(A;;FR;;;WD)"},
		{"users", "D:P(A;;FA;;;" + user.String() + ")(A;;FR;;;BU)"},
		{"authenticated", "D:P(A;;FA;;;" + user.String() + ")(A;;FR;;;AU)"},
		{"inherited", "D:P(A;;FA;;;" + user.String() + ")(A;ID;FR;;;WD)"},
		{"null", "D:NO_ACCESS_CONTROL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testDirectory(t)
			if err := Replace(dir, "credential", []byte("secret")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "credential")
			setDACL(t, path, tc.sddl)
			if err := Check(path, false); err == nil {
				t.Fatal("accepted unsafe DACL")
			}
			if _, err := ReadFile(path, 64); err == nil {
				t.Fatal("read unsafe DACL")
			}
			if f, err := Lock(path); err == nil {
				_ = UnlockClose(f)
				t.Fatal("locked unsafe DACL")
			}
			if err := Replace(dir, "credential", []byte("changed")); err == nil {
				t.Fatal("replaced unsafe credential")
			}
			bytes, err := os.ReadFile(path)
			if err != nil || string(bytes) != "secret" {
				t.Fatalf("credential changed: %q %v", bytes, err)
			}
		})
	}
}

func TestWindowsRejectsWrongOwnerWhenPrivilegeAvailable(t *testing.T) {
	dir := testDirectory(t)
	path := filepath.Join(dir, "credential")
	if err := Replace(dir, "credential", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, system, nil, nil, nil); err != nil {
		t.Skipf("wrong-owner setup requires owner-assignment privilege: %v", err)
	}
	if err = Check(path, false); err == nil {
		t.Fatal("accepted another owner")
	}
	if _, err = ReadFile(path, 64); err == nil {
		t.Fatal("read another owner's credential")
	}
}

func TestWindowsLockCannotBeReplaced(t *testing.T) {
	dir := testDirectory(t)
	path := filepath.Join(dir, "owner.lock")
	f, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = UnlockClose(f) }()
	if err = os.Remove(path); err == nil {
		t.Fatal("deleted retained lock file")
	}
	if err = Replace(dir, "owner.lock", []byte("new lock")); err == nil {
		t.Fatal("replaced retained lock file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "owner.lock" {
		t.Fatalf("failed replacement leaked files: %v", entries)
	}
}

func TestWindowsRejectsJunctionAncestor(t *testing.T) {
	dir := testDirectory(t)
	target := filepath.Join(dir, "target")
	if err := EnsureDir(target); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, "credential", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "junction")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %q %v", output, err)
	}
	if err := Check(link, true); err == nil {
		t.Fatal("accepted junction")
	}
	if _, err := ReadFile(filepath.Join(link, "credential"), 64); err == nil {
		t.Fatal("read through junction")
	}
	if err := EnsureDir(filepath.Join(link, "state")); err == nil {
		t.Fatal("created directory through junction")
	}
}
