package cluster

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/jeessy2/ddns-go/v6/config"
)

func ValidateIP(ip config.ResolvedIP) error {
	if ip.IPv4 != "" {
		a, e := netip.ParseAddr(ip.IPv4)
		if e != nil || !a.Is4() {
			return errors.New("无效 IPv4")
		}
	}
	if ip.IPv6 != "" {
		a, e := netip.ParseAddr(ip.IPv6)
		if e != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return errors.New("无效 IPv6")
		}
	}
	return nil
}
func Prefix(address string, bits int) (string, error) {
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" || bits < 1 || bits > 128 {
		return "", errors.New("无效 IPv6 地址或前缀长度")
	}
	return netip.PrefixFrom(a, bits).Masked().String(), nil
}

// Compose replaces exactly the network bits, retaining all host bits (including /57, /73).
func Compose(prefix, address string) (string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
		return "", errors.New("无效 IPv6 前缀")
	}
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
		return "", errors.New("无效 IPv6 后缀")
	}
	network := p.Masked().Addr().As16()
	host := a.As16()
	for i := 0; i < 16; i++ {
		bits := p.Bits() - 8*i
		if bits > 8 {
			bits = 8
		}
		if bits < 0 {
			bits = 0
		}
		mask := byte(0xff << uint(8-bits))
		host[i] = (network[i] & mask) | (host[i] &^ mask)
	}
	return netip.AddrFrom16(host).String(), nil
}

type State struct {
	LayoutHash    string
	Prefix        string
	Addresses     map[string]string
	Reports       map[string]string
	PendingPrefix string
	Pending       []Job
}

// State is separate from user YAML so background updates cannot overwrite Web edits.
func LoadState(path string) (map[string]*State, error) {
	states := map[string]*State{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &states); err != nil {
		return nil, err
	}
	if states == nil {
		states = map[string]*State{}
	}
	return states, nil
}
func SaveState(path string, s map[string]*State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cluster-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
