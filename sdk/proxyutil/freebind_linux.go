//go:build linux

package proxyutil

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func bindDialerControl(localAddr net.Addr) func(network, address string, c syscall.RawConn) error {
	tcpAddr, ok := localAddr.(*net.TCPAddr)
	if !ok || tcpAddr == nil || tcpAddr.IP == nil {
		return nil
	}
	if tcpAddr.IP.To4() != nil {
		return func(_ string, _ string, rawConn syscall.RawConn) error {
			return controlFreeBind(rawConn, unix.IPPROTO_IP, unix.IP_FREEBIND)
		}
	}
	return func(_ string, _ string, rawConn syscall.RawConn) error {
		return controlFreeBind(rawConn, unix.IPPROTO_IPV6, unix.IPV6_FREEBIND)
	}
}

func controlFreeBind(rawConn syscall.RawConn, level, option int) error {
	if rawConn == nil {
		return nil
	}
	var controlErr error
	if err := rawConn.Control(func(fd uintptr) {
		controlErr = unix.SetsockoptInt(int(fd), level, option, 1)
	}); err != nil {
		return err
	}
	return controlErr
}
