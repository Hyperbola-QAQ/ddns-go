//go:build linux

package config

import (
	"encoding/binary"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func ipv6Lifetimes() map[string]AddressLifetime {
	data, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_INET6)
	if err != nil {
		return nil
	}
	messages, err := syscall.ParseNetlinkMessage(data)
	if err != nil {
		return nil
	}
	return parseLifetimes(messages)
}
func parseLifetimes(messages []syscall.NetlinkMessage) map[string]AddressLifetime {
	result := map[string]AddressLifetime{}
	for _, m := range messages {
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < 8 || m.Data[0] != syscall.AF_INET6 {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			continue
		}
		index := binary.NativeEndian.Uint32(m.Data[4:8])
		flags := uint32(m.Data[2])
		var address string
		var life AddressLifetime
		for _, a := range attrs {
			switch a.Attr.Type {
			case syscall.IFA_ADDRESS:
				if len(a.Value) >= 16 {
					address = net.IP(a.Value[:16]).String()
				}
			case syscall.IFA_CACHEINFO:
				if len(a.Value) >= 8 {
					life.PreferredSeconds = binary.NativeEndian.Uint32(a.Value[:4])
					life.ValidSeconds = binary.NativeEndian.Uint32(a.Value[4:8])
					life.Known = true
				}
			case unix.IFA_FLAGS:
				if len(a.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(a.Value[:4])
				}
			}
		}
		life.Unusable = flags&(syscall.IFA_F_TENTATIVE|syscall.IFA_F_DADFAILED) != 0 || (life.Known && life.ValidSeconds == 0)
		if address != "" {
			result[strconv.Itoa(int(index))+"/"+address] = life
		}
	}
	return result
}
