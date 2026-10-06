//go:build !linux

package main

import (
	"errors"
	"syscall"
)

func wireGuardDeviceControl(interfaceName string) (func(string, string, syscall.RawConn) error, error) {
	if !wgInterfaceName.MatchString(interfaceName) {
		return nil, errors.New("invalid WireGuard interface name")
	}
	return func(_ string, _ string, _ syscall.RawConn) error {
		return errors.New("WireGuard relay requires Linux")
	}, nil
}

func bindWireGuard(network, address string, connection syscall.RawConn) error {
	control, _ := wireGuardDeviceControl("wg0")
	return control(network, address, connection)
}
