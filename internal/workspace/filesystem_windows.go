//go:build windows

package workspace

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func inspectFilesystemPath(path string, _ os.FileInfo) (uint64, bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, false, err
	}
	defer windows.CloseHandle(handle)
	var details windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &details); err != nil {
		return 0, false, err
	}
	if details.VolumeSerialNumber == 0 {
		return 0, false, errors.New("workspace: filesystem identity unavailable")
	}
	return uint64(details.VolumeSerialNumber), details.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func capturedFileMode(_ os.FileInfo, indexMode int64, tracked bool) int64 {
	if tracked && indexMode == 0100755 {
		return 0755
	}
	return 0644
}

func gitPlatformEnvironment() []string {
	env := make([]string, 0, 4)
	for _, name := range []string{"SystemRoot", "ComSpec", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
