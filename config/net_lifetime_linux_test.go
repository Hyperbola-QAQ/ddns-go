//go:build linux

package config

import (
	"encoding/binary"
	"golang.org/x/sys/unix"
	"net"
	"syscall"
	"testing"
)

func TestIPv6LifetimeOrderAndUnusableAddresses(t *testing.T) {
	attr := func(kind uint16, value []byte) []byte {
		size := 4 + len(value)
		b := make([]byte, (size+3)&^3)
		binary.NativeEndian.PutUint16(b, uint16(size))
		binary.NativeEndian.PutUint16(b[2:], kind)
		copy(b[4:], value)
		return b
	}
	message := func(address string, valid, preferred, flags uint32) syscall.NetlinkMessage {
		data := make([]byte, 8)
		data[0] = syscall.AF_INET6
		binary.NativeEndian.PutUint32(data[4:], 2)
		data = append(data, attr(syscall.IFA_ADDRESS, net.ParseIP(address).To16())...)
		life := make([]byte, 16)
		binary.NativeEndian.PutUint32(life, preferred)
		binary.NativeEndian.PutUint32(life[4:], valid)
		data = append(data, attr(syscall.IFA_CACHEINFO, life)...)
		f := make([]byte, 4)
		binary.NativeEndian.PutUint32(f, flags)
		data = append(data, attr(unix.IFA_FLAGS, f)...)
		return syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: syscall.RTM_NEWADDR}, Data: data}
	}
	lifetimes := parseLifetimes([]syscall.NetlinkMessage{message("2001:db8::1", 60, 30, 0), message("2001:db8::2", 600, 300, 0), message("2001:db8::3", 600, 400, 0), message("2001:db8::4", 0, 0, 0), message("2001:db8::5", 1000, 1000, syscall.IFA_F_TENTATIVE)})
	ips := []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}
	sortIPv6(ips, 2, lifetimes)
	if ips[0] != "2001:db8::3" || ips[1] != "2001:db8::2" {
		t.Fatal("longest valid lifetime not selected", ips)
	}
	if !lifetimes["2/2001:db8::4"].Unusable || !lifetimes["2/2001:db8::5"].Unusable {
		t.Fatal("expired/tentative IPv6 accepted")
	}
}
