package cluster

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	Version         config.SyncVersion
	LastKey         string
	LastAt          time.Time
	LayoutHash      string
	Prefix          string
	PrefixAt        time.Time
	Retired         map[string]time.Time
	CooldownSeconds int
	Addresses       map[string]string
	Reports         map[string]string
	PendingPrefix   string
	Pending         []Job
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

var stateMu sync.Mutex

// SaveGroup merges with the latest file so a peer cannot overwrite another group's state.
func SaveGroup(path, group string, state *State) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	all, err := LoadState(path)
	if err != nil {
		return err
	}
	all[group] = state
	return SaveState(path, all)
}

// CandidatePrefix accepts a new network immediately, but suppresses delayed returns
// to retired networks. A persistent mixed-prefix group needs operator correction.
func (s *State) CandidatePrefix(reports map[string]config.ResolvedIP, bits int, now time.Time) (string, error) {
	prefixes := map[string]bool{}
	for _, ip := range reports {
		if ip.IPv6 != "" {
			p, err := Prefix(ip.IPv6, bits)
			if err != nil {
				return "", err
			}
			prefixes[p] = true
		}
	}
	candidates := map[string]bool{}
	for p := range prefixes {
		if p == s.Prefix {
			continue
		}
		if until, old := s.Retired[p]; old {
			if now.Before(until) {
				continue
			}
			if len(prefixes) > 1 {
				return "", errors.New("同一配置的 IPv6 前缀持续不一致，请检查节点网络；已暂停前缀更新")
			}
		}
		candidates[p] = true
	}
	if len(candidates) > 1 {
		return "", errors.New("同一配置的节点上报多个不同 IPv6 前缀，请确保属于同一前缀变化域")
	}
	for p := range candidates {
		return p, nil
	}
	return s.Prefix, nil
}
func (s *State) CommitPrefix(prefix string, base int, now time.Time) {
	if prefix == s.Prefix {
		return
	}
	if base <= 0 {
		base = 60
	}
	cooldown := base
	if !s.PrefixAt.IsZero() && now.Sub(s.PrefixAt) < time.Duration(s.CooldownSeconds)*time.Second {
		cooldown = s.CooldownSeconds * 2
		if cooldown > base*4 {
			cooldown = base * 4
		}
	}
	if s.Retired == nil {
		s.Retired = map[string]time.Time{}
	}
	if s.Prefix != "" {
		s.Retired[s.Prefix] = now.Add(time.Duration(cooldown) * time.Second)
	}
	// Extend the recent-history window after rapid consecutive changes.
	for p, until := range s.Retired {
		if now.Before(until) {
			s.Retired[p] = now.Add(time.Duration(cooldown) * time.Second)
		} else if now.Sub(until) > 24*time.Hour {
			delete(s.Retired, p)
		}
	}
	s.Prefix = prefix
	s.PrefixAt = now
	s.CooldownSeconds = cooldown
}
