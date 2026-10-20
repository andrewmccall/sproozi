package home

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestWindowsControlPipeSecurity(t *testing.T) {
	address := controlAddress(t)
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	conn, err := dialControl(controlContext(t), address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not accept")
	}
	defer func() { _ = server.Close() }()
	handle := windows.Handle(server.(interface{ Fd() uintptr }).Fd())
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	user, err := controlUser()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(user) {
		t.Fatal("pipe is not owned by current user")
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("pipe DACL allows inherited access")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if acl == nil || acl.AceCount != 2 {
		t.Fatalf("expected only current-user and SYSTEM ACEs: %+v", acl)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 || ace.Mask == 0 {
			t.Fatalf("unexpected pipe ACE: %+v", ace)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user):
			seenUser = true
		case sid.Equals(system):
			seenSystem = true
		default:
			t.Fatalf("pipe admits another SID: %s", sid)
		}
	}
	if !seenUser || !seenSystem {
		t.Fatal("pipe lacks explicit user/SYSTEM access")
	}
	if duplicate, err := listenControl(address); err == nil {
		_ = duplicate.Close()
		t.Fatal("second pipe server admitted")
	}
}

func TestWindowsControlRejectsSquattedAddress(t *testing.T) {
	address := controlAddress(t)
	// A pre-existing listener with a permissive ACL must never be replaced by a
	// later home daemon. Same-user access is allowed; the OS still excludes a
	// second server's first-instance creation.
	squatter, err := winio.ListenPipe(address, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;WD)"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	if listener, err := listenControl(address); err == nil {
		_ = listener.Close()
		t.Fatal("home replaced pre-existing pipe")
	}
}

func TestWindowsControlServerIdentityFailsBeforeRequest(t *testing.T) {
	address := controlAddress(t)
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	observed := make(chan int, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			observed <- -1
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		data, _ := io.ReadAll(conn)
		observed <- len(data)
	}()
	conn, err := winio.DialPipeContext(controlContext(t), address)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyControlServer(conn, foreign); err == nil {
		_ = conn.Close()
		t.Fatal("another account's expected SID accepted")
	}
	_ = conn.Close()
	if n := <-observed; n != 0 {
		t.Fatalf("unverified server received %d application bytes", n)
	}
}

func TestWindowsControlRejectsNonlocalAddresses(t *testing.T) {
	for _, address := range []string{"", `\\server\pipe\sproozi`, `\\.\pipe\`, `\\.\pipe\a\b`, "tcp://localhost:1234", `C:\control.sock`} {
		if listener, err := listenControl(address); err == nil {
			_ = listener.Close()
			t.Fatalf("accepted listener %q", address)
		}
		if conn, err := dialControl(context.Background(), address); err == nil {
			_ = conn.Close()
			t.Fatalf("accepted dial %q", address)
		}
	}
}

func TestWindowsControlCanonicalAddress(t *testing.T) {
	directory := t.TempDir()
	want := SocketPath(directory)
	if want == "" || want != SocketPath(strings.ToUpper(directory)) || want != SocketPath(filepath.Join(directory, ".")) {
		t.Fatal("equivalent home paths have different control addresses")
	}
	if want == SocketPath(directory+"-different") {
		t.Fatal("different homes share a control address")
	}
}

// This test provisions an actual second local account when the runner permits
// account creation and CreateProcessAsUser. Skips identify an unavailable
// privilege-dependent proof; the always-on ACL and identity tests above remain.
func TestWindowsControlRejectsOtherUserBeforeRequest(t *testing.T) {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(entropy[:])
	account, password := "sc"+suffix[:12], "Sc!"+suffix+"z9"
	if err := exec.Command("net.exe", "user", account, password, "/add").Run(); err != nil {
		t.Skipf("runner cannot provision a second local account: %v", err)
	}
	defer func() { _ = exec.Command("net.exe", "user", account, "/delete").Run() }()
	token, err := controlTestLogon(account, password)
	if err != nil {
		t.Fatalf("log on fixture account: %v", err)
	}
	defer func() { _ = token.Close() }()
	public := os.Getenv("PUBLIC")
	if public == "" {
		t.Fatal("PUBLIC directory unavailable for cross-user executable")
	}
	directory, err := os.MkdirTemp(public, "sproozi-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(directory, "control-test.exe")
	if err = os.WriteFile(helper, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Public's inherited ACL permits the fixture account to execute this copy.
	// No credentials or private test data are written alongside it.
	address := SocketPath(directory)
	ctx := controlContext(t)
	cmd := exec.CommandContext(ctx, helper, "-test.run=^TestWindowsControlOtherUserHelper$", "-test.count=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(token)}
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "PATH=" + os.Getenv("PATH"), "SPROOZI_CONTROL_OTHER_USER=1", "SPROOZI_CONTROL_ADDRESS=" + address}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skipf("runner lacks CreateProcessAsUser privilege: %v", err)
		}
		t.Fatalf("start second-user fixture: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(output)
	ready, err := reader.ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("second-user server failed to start: %q, %v", ready, err)
	}
	if conn, _, err := connect(ctx, address, Request{Action: ActionStatus, ID: controlTestID}); err == nil {
		_ = conn.Close()
		t.Fatal("connected to a different user's squatting pipe")
	}
	observed, err := reader.ReadString('\n')
	if err != nil || observed != "bytes=0\n" {
		t.Fatalf("unverified server observed application data: %q, %v", observed, err)
	}
}

func controlTestLogon(account, password string) (windows.Token, error) {
	user, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return 0, err
	}
	domain, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return 0, err
	}
	secret, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return 0, err
	}
	var token windows.Token
	result, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW").Call(uintptr(unsafe.Pointer(user)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(secret)), 2, 0, uintptr(unsafe.Pointer(&token)))
	if result == 0 {
		return 0, callErr
	}
	return token, nil
}

func TestWindowsControlOtherUserHelper(t *testing.T) {
	if os.Getenv("SPROOZI_CONTROL_OTHER_USER") != "1" {
		return
	}
	listener, err := winio.ListenPipe(os.Getenv("SPROOZI_CONTROL_ADDRESS"), &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;WD)"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	fmt.Println("ready")
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	fmt.Printf("bytes=%d\n", len(data))
}

// A control pipe another process still serves is refused, not replaced.
func TestWindowsControlRefusesLivePipe(t *testing.T) {
	address := controlAddress(t)
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if other, err := listenControl(address); !errors.Is(err, errControlListening) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second listener on a live pipe: %v", err)
	}
}
