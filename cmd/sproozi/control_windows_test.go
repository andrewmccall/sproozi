package main

import (
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func listenTestControl(address string) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	descriptor := "O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
	return winio.ListenPipe(address, &winio.PipeConfig{SecurityDescriptor: descriptor})
}
