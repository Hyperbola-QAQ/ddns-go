package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestLegacyClusterDefaults(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("dnsconf:\n  - name: original\n    ipv4:\n      enable: true\n      gettype: url\n"), &c); err != nil {
		t.Fatal(err)
	}
	c.ClusterDefaults()
	p := c.DnsConf[0].Cluster
	if p.Enabled || p.PrefixLength != 64 || p.Source != "node" || p.SourceNode != "local" || p.Algorithm != "random" {
		t.Fatalf("bad defaults: %+v", p)
	}
	if err := c.ValidateCluster(); err != nil {
		t.Fatal(err)
	}
}
func TestClusterValidation(t *testing.T) {
	c := Config{Cluster: ClusterSettings{LocalID: "local"}, DnsConf: []DnsConfig{{Name: "g", Cluster: ClusterPolicy{Enabled: true, Group: "g"}}}}
	c.ClusterDefaults()
	if err := c.ValidateCluster(); err != nil {
		t.Fatal(err)
	}
	c.DnsConf[0].Cluster.Source = "prefix"
	c.DnsConf[0].Cluster.Suffix = "2001:db8::1"
	if err := c.ValidateCluster(); err == nil {
		t.Fatal("oversized suffix accepted")
	}
	c.DnsConf[0].Cluster.Suffix = "::1"
	if err := c.ValidateCluster(); err != nil {
		t.Fatal(err)
	}
	c.Cluster.Nodes = []ClusterNode{{ID: "local", IPv6Domains: "host.example.com"}}
	c.DnsConf[0].Ipv6.Domains = []string{"host.example.com"}
	if err := c.ValidateCluster(); err == nil {
		t.Fatal("overlapping normal/batch records accepted")
	}
}
func TestSnapshotIsolation(t *testing.T) {
	c := Config{DnsConf: []DnsConfig{{Name: "original"}}, Cluster: ClusterSettings{Nodes: []ClusterNode{{ID: "a"}}}}
	c.DnsConf[0].Ipv6.Domains = []string{"a.example.com"}
	copy := cloneConfig(c)
	copy.DnsConf[0].Name = "changed"
	copy.DnsConf[0].Ipv6.Domains[0] = "b.example.com"
	copy.Cluster.Nodes[0].ID = "b"
	if c.DnsConf[0].Name != "original" || c.DnsConf[0].Ipv6.Domains[0] != "a.example.com" || c.Cluster.Nodes[0].ID != "a" {
		t.Fatal("snapshot aliases cache")
	}
}

func TestNodeNetworkSuffixValidationAndClone(t *testing.T) {
	c := Config{Cluster: ClusterSettings{LocalID: "self"}, DnsConf: []DnsConfig{{ID: "g", Networks: []NodeNetwork{{NodeID: "self", Ipv6: IPConfig{Enable: true, GetType: "cmd", Domains: []string{"a.example.com", "b.example.com"}}, Suffixes: "\n::1000"}}}}}
	c.ClusterDefaults()
	if err := c.ValidateCluster(); err != nil {
		t.Fatal("blank first suffix rejected", err)
	}
	copy := cloneConfig(c)
	copy.DnsConf[0].Networks[0].Ipv6.Domains[0] = "other.example.com"
	if c.DnsConf[0].Networks[0].Ipv6.Domains[0] != "a.example.com" {
		t.Fatal("network snapshot shares domain slice")
	}
	c.DnsConf[0].Networks[0].Suffixes = "::10001"
	if err := c.ValidateCluster(); err == nil {
		t.Fatal("invalid five-digit IPv6 segment accepted")
	}
}

func TestNodeNetworkSuffixYAMLRoundTrip(t *testing.T) {
	for _, suffix := range []string{"\n::1000", "::1\n::1000", "::1\n", ""} {
		c := Config{DnsConf: []DnsConfig{{Networks: []NodeNetwork{{NodeID: "a", Suffixes: suffix}}}}}
		raw, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var saved Config
		if err := yaml.Unmarshal(raw, &saved); err != nil {
			t.Fatalf("saved YAML cannot be loaded: %v", err)
		}
		if saved.DnsConf[0].Networks[0].Suffixes != suffix {
			t.Fatal("suffix blank lines lost")
		}
	}
}

func TestGlobalSuffixYAMLAndValidation(t *testing.T) {
	c := Config{Cluster: ClusterSettings{LocalID: "a"}, DnsConf: []DnsConfig{{ID: "g", IPv6Suffix: &SuffixConfig{Enable: true, IPs: "\n::1000"}, Networks: []NodeNetwork{{NodeID: "a", Ipv6: IPConfig{Domains: []string{"a.example.com", "b.example.com"}}}}}}}
	c.ClusterDefaults()
	if err := c.ValidateCluster(); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := yaml.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DnsConf[0].IPv6Suffix.IPs != "\n::1000" {
		t.Fatal("global suffix line order lost")
	}
	c.DnsConf[0].IPv6Suffix.IPs = "::1\n::2\n::3"
	if err := c.ValidateCluster(); err == nil {
		t.Fatal("too many suffixes accepted")
	}
}
