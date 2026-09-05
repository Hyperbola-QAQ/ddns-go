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
