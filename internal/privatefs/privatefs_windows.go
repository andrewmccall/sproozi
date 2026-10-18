//go:build windows

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

// The descriptor is protected from parent inheritance. Child directories and
// files inherit only the user's and SYSTEM's access.
func descriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
}

func attributes(sd *windows.SECURITY_DESCRIPTOR) *windows.SecurityAttributes {
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
}

// noReparse rejects every existing path component, including junctions. State
// is limited to local drive paths; UNC, device and remote drive paths fail closed.
func noReparse(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' || strings.Contains(path[len(volume):], ":") {
		return unsafePath(path)
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return err
	}
	if windows.GetDriveType(root) != windows.DRIVE_FIXED {
		return unsafePath(path)
	}
	for {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(p)
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return &os.PathError{Op: "inspect", Path: path, Err: err}
		}
		if err == nil && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return unsafePath(path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return nil
}

func checkHandle(path string, handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return unsafePath(path)
	}
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return err
	}
	if kind != windows.FILE_TYPE_DISK {
		return unsafePath(path)
	}
	var filesystem [32]uint16
	if err = windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return err
	}
	if !strings.EqualFold(windows.UTF16ToString(filesystem[:]), "NTFS") {
		return errors.New("privatefs: Windows private state requires local NTFS")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	sid, err := currentUser()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return unsafePath(path)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return unsafePath(path)
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !trustee.IsValid() || (!trustee.Equals(sid) && !trustee.IsWellKnown(windows.WinLocalSystemSid) && !trustee.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
				return unsafePath(path)
			}
		default:
			// Object/callback/conditional grants are outside the supported ACL policy.
			return unsafePath(path)
		}
	}
	runtime.KeepAlive(sd)
	return nil
}

// Check validates the opened object's owner, DACL and reparse attributes.
func Check(path string, directory bool) error {
	if err := noReparse(path); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return checkHandle(path, h, directory)
}

// EnsureDir creates missing components with a protected ACL at creation and
// checks the final directory. It never changes an existing object's ACL.
func EnsureDir(path string) error {
	if err := noReparse(path); err != nil {
		return err
	}
	if err := makeParents(path); err != nil {
		return err
	}
	return Check(path, true)
}
func makeParents(path string) error {
	err := mkdir(path)
	if err == nil || os.IsExist(err) {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := makeParents(parent); err != nil {
		return err
	}
	err = mkdir(path)
	if os.IsExist(err) {
		return nil
	}
	return err
}
func mkdir(path string) error {
	if err := noReparse(path); err != nil {
		return err
	}
	sd, err := descriptor()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(p, attributes(sd))
	runtime.KeepAlive(sd)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

func openFile(path string, create bool) (*os.File, error) {
	access, disposition := uint32(windows.GENERIC_READ|windows.READ_CONTROL), uint32(windows.OPEN_EXISTING)
	if create {
		access |= windows.GENERIC_WRITE
		disposition = windows.OPEN_ALWAYS
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if !create {
		share |= windows.FILE_SHARE_DELETE
	}
	return open(path, access, disposition, share)
}
func createFile(path string) (*os.File, error) {
	return open(path, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, windows.CREATE_NEW, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
}
func open(path string, access, disposition, share uint32) (*os.File, error) {
	if err := noReparse(path); err != nil {
		return nil, err
	}
	sd, err := descriptor()
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Locks omit FILE_SHARE_DELETE; ordinary readers permit atomic replacement.
	h, err := windows.CreateFile(p, access, share, attributes(sd), disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err = checkHandle(path, h, false); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// Lock acquires a nonblocking exclusive byte-range lock on the validated handle.
func Lock(path string) (*os.File, error) {
	if err := Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	f, err := openFile(path, true)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func UnlockClose(f *os.File) error {
	return errors.Join(windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{}), f.Close())
}

func replaceFile(temp, target, _ string) error {
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// Same-volume rename preserves the private temporary file's DACL. Windows has
	// no Unix parent-directory Sync equivalent; request documented write-through.
	if err = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.PathError{Op: "replace", Path: target, Err: err}
	}
	return Check(target, false)
}
