package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jeessy2/ddns-go/v6/config"
)

// LayoutHash catches divergent group/domain mappings without exchanging provider secrets.
func LayoutHash(c config.Config, d config.DnsConfig) string {
	type binding struct {
		ID      string
		Suffix  string
		Domains string
	}
	var nodes []binding
	for _, id := range d.Cluster.IDs() {
		n, _ := c.Node(id)
		nodes = append(nodes, binding{id, n.Suffix, n.IPv6Domains})
	}
	b, _ := json.Marshal(struct {
		Policy config.ClusterPolicy
		Nodes  []binding
		IPv4   []string
		IPv6   []string
		V4     bool
		V6     bool
	}{d.Cluster, nodes, d.Ipv4.Domains, d.Ipv6.Domains, d.Ipv4.Enable, d.Ipv6.Enable})
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}
