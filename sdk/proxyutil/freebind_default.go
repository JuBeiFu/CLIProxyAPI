//go:build !linux

package proxyutil

import (
	"net"
	"syscall"
)

func bindDialerControl(_ net.Addr) func(network, address string, c syscall.RawConn) error {
	return nil
}
