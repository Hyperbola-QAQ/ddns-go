package cluster

import (
	"github.com/jeessy2/ddns-go/v6/config"
	"path/filepath"
	"testing"
	"time"
)

func TestPrefixCooldownAndDelayedReports(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	s := State{}
	s.CommitPrefix("2001:db8:1::/64", 60, now)
	report := func(a, b string) map[string]config.ResolvedIP {
		return map[string]config.ResolvedIP{"a": {IPv6: a}, "b": {IPv6: b}}
	}
	next, err := s.CandidatePrefix(report("2001:db8:2::1", "2001:db8:1::2"), 64, now.Add(10*time.Second))
	if err != nil || next != "2001:db8:2::/64" {
		t.Fatal("new prefix should update immediately", next, err)
	}
	s.CommitPrefix(next, 60, now.Add(10*time.Second))
	if s.CooldownSeconds != 120 {
		t.Fatal("rapid change did not extend cooldown")
	}
	for _, ips := range []map[string]config.ResolvedIP{report("2001:db8:1::1", "2001:db8:2::2"), report("2001:db8:1::1", "2001:db8:1::2")} {
		got, err := s.CandidatePrefix(ips, 64, now.Add(30*time.Second))
		if err != nil || got != s.Prefix {
			t.Fatal("delayed old prefix reverted DNS", got, err)
		}
	}
	if _, err := s.CandidatePrefix(report("2001:db8:1::1", "2001:db8:2::2"), 64, now.Add(131*time.Second)); err == nil {
		t.Fatal("persistent mixed networks must not oscillate")
	}
	got, err := s.CandidatePrefix(report("2001:db8:1::1", "2001:db8:1::2"), 64, now.Add(131*time.Second))
	if err != nil || got != "2001:db8:1::/64" {
		t.Fatal("stable real rollback after cooldown rejected", got, err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, map[string]*State{"g": &s}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil || !loaded["g"].PrefixAt.Equal(s.PrefixAt) || len(loaded["g"].Retired) != 1 {
		t.Fatal("cooldown not persisted")
	}
}
