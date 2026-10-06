//go:build linux

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWireGuardSocketCannotFallBackWithoutInterface(t *testing.T) {
	if _, err := net.InterfaceByName("wg0"); err == nil {
		t.Skip("fixture requires no wg0 interface")
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	client := &http.Client{Transport: wireGuardTransport(func() []string { return []string{"162.252.172.57"} })}
	response, err := client.Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || hits.Load() != 0 {
		t.Fatal("missing wg0 allowed ordinary network fallback")
	}
}

func TestWireGuardNamedSocketCannotFallBackWithoutInterface(t *testing.T) {
	const interfaceName = "wg-nofallback"
	if _, err := net.InterfaceByName(interfaceName); err == nil {
		t.Skip("fixture requires missing named interface")
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	transport, err := wireGuardTransportForInterface(interfaceName, func() []string { return []string{"162.252.172.57"} })
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || hits.Load() != 0 {
		t.Fatal("missing named WireGuard interface allowed ordinary network fallback")
	}
}
