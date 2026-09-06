package config

import (
	"fmt"
	"net"
	"sort"
	"strconv"
)

// NetInterface 本机网络
type NetInterface struct {
	Name      string
	Address   []string
	Lifetimes []AddressLifetime
}

type AddressLifetime struct {
	Known                          bool
	PreferredSeconds, ValidSeconds uint32
	Unusable                       bool
}

// GetNetInterface 获得网卡地址
// 返回ipv4, ipv6地址
func GetNetInterface() (ipv4NetInterfaces []NetInterface, ipv6NetInterfaces []NetInterface, err error) {
	lifetimes := ipv6Lifetimes()
	allNetInterfaces, err := net.Interfaces()
	if err != nil {
		fmt.Println("net.Interfaces failed, err:", err.Error())
		return ipv4NetInterfaces, ipv6NetInterfaces, err
	}

	// https://en.wikipedia.org/wiki/IPv6_address#General_allocation
	_, ipv6Unicast, _ := net.ParseCIDR("2000::/3")

	for i := 0; i < len(allNetInterfaces); i++ {
		if (allNetInterfaces[i].Flags & net.FlagUp) != 0 {
			addrs, _ := allNetInterfaces[i].Addrs()
			ipv4 := []string{}
			ipv6 := []string{}

			for _, address := range addrs {
				if ipnet, ok := address.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
					_, bits := ipnet.Mask.Size()
					// 需匹配全局单播地址
					if bits == 128 && ipv6Unicast.Contains(ipnet.IP) {
						if !lifetimes[strconv.Itoa(allNetInterfaces[i].Index)+"/"+ipnet.IP.String()].Unusable {
							ipv6 = append(ipv6, ipnet.IP.String())
						}
					}
					if bits == 32 {
						ipv4 = append(ipv4, ipnet.IP.String())
					}
				}
			}

			if len(ipv4) > 0 {
				ipv4NetInterfaces = append(
					ipv4NetInterfaces,
					NetInterface{
						Name:    allNetInterfaces[i].Name,
						Address: ipv4,
					},
				)
			}

			if len(ipv6) > 0 {
				sortIPv6(ipv6, allNetInterfaces[i].Index, lifetimes)
				lives := make([]AddressLifetime, len(ipv6))
				for j, ip := range ipv6 {
					lives[j] = lifetimes[strconv.Itoa(allNetInterfaces[i].Index)+"/"+ip]
				}
				ipv6NetInterfaces = append(
					ipv6NetInterfaces,
					NetInterface{
						Name:      allNetInterfaces[i].Name,
						Address:   ipv6,
						Lifetimes: lives,
					},
				)
			}

		}
	}

	return ipv4NetInterfaces, ipv6NetInterfaces, nil
}

func sortIPv6(addresses []string, index int, lifetimes map[string]AddressLifetime) {
	sort.SliceStable(addresses, func(i, j int) bool {
		a := lifetimes[strconv.Itoa(index)+"/"+addresses[i]]
		b := lifetimes[strconv.Itoa(index)+"/"+addresses[j]]
		if a.Known != b.Known {
			return a.Known
		}
		if a.ValidSeconds != b.ValidSeconds {
			return a.ValidSeconds > b.ValidSeconds
		}
		return a.PreferredSeconds > b.PreferredSeconds
	})
}
