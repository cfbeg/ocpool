//go:build linux

package main

import (
	"errors"
	"syscall"
)

func wireGuardDeviceControl(interfaceName string) (func(string, string, syscall.RawConn) error, error) {
	if !wgInterfaceName.MatchString(interfaceName) {
		return nil, errors.New("invalid WireGuard interface name")
	}
	return func(_ string, _ string, connection syscall.RawConn) error {
		var socketErr error
		if err := connection.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, interfaceName)
		}); err != nil {
			return err
		}
		return socketErr
	}, nil
}

func bindWireGuard(network, address string, connection syscall.RawConn) error {
	control, _ := wireGuardDeviceControl("wg0")
	return control(network, address, connection)
}
