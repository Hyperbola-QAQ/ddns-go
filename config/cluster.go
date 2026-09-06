package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
)

var clusterAlgorithms sync.Map

func init() { RegisterClusterAlgorithm("random"); RegisterClusterAlgorithm("round-robin") }

// RegisterClusterAlgorithm is called by the selector registry for configuration validation.
func RegisterClusterAlgorithm(name string) { clusterAlgorithms.Store(name, true) }

// ClusterPolicy is opt-in; an absent policy preserves the single-node updater.
type ClusterPolicy struct {
	Enabled         bool
	Group           string `yaml:"-" json:"-"`               // derived from DnsConfig.ID
	LegacyGroup     string `yaml:"group,omitempty" json:"-"` // old state-file key
	NodeIDs         string // comma-separated stable IDs, not display names
	Coordinator     string `yaml:"coordinator,omitempty" json:"-"` // read old files; no scheduling authority
	Source          string // node, auto, prefix
	SourceNode      string
	Algorithm       string // random, round-robin
	PrefixLength    int
	CooldownSeconds int
	Suffix          string
}

type ClusterNode struct {
	ID          string
	Name        string
	URL         string
	Group       string `yaml:"-" json:"-"` // ignored legacy node grouping
	Token       string // token accepted by this peer
	Suffix      string
	IPv6Domains string // one record per line for prefix-change batches
}

type ClusterSettings struct {
	LocalID string
	Token   string // inbound peer token, independent of the Web login
	Nodes   []ClusterNode
	Version SyncVersion
}

// SyncVersion is a logical clock; concurrent versions use node ID as a deterministic tie-break.
type SyncVersion struct {
	Counter uint64
	Node    string
	Actor   string
}

func (v SyncVersion) Newer(other SyncVersion) bool {
	return v.Counter > other.Counter || (v.Counter == other.Counter && v.Node > other.Node)
}

func (p ClusterPolicy) IDs() []string {
	var ids []string
	for _, id := range strings.Split(p.NodeIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *ClusterPolicy) Defaults(local string) {
	if p.Source == "" {
		p.Source = "node"
	}
	if p.SourceNode == "" {
		p.SourceNode = "local"
	}
	if p.Algorithm == "" {
		p.Algorithm = "random"
	}
	if p.PrefixLength == 0 {
		p.PrefixLength = 64
	}
	if p.CooldownSeconds == 0 {
		p.CooldownSeconds = 60
	}
	p.Coordinator = ""
	if p.NodeIDs == "" {
		p.NodeIDs = local
	}
}

func (c *Config) ClusterDefaults() {
	if c.Cluster.LocalID == "" {
		c.Cluster.LocalID = strings.ToLower(rand.Text()[:12])
		c.identityGenerated = true
	}
	if c.Cluster.Token == "" {
		c.Cluster.Token = rand.Text() + rand.Text()
		c.identityGenerated = true
	}
	for i := range c.Cluster.Nodes {
		if c.Cluster.Nodes[i].Name == "" {
			c.Cluster.Nodes[i].Name = c.Cluster.Nodes[i].ID
		}
	}
	for i := range c.DnsConf {
		if c.DnsConf[i].ID == "" {
			c.DnsConf[i].ID = rand.Text()
		}
		c.DnsConf[i].Cluster.Defaults(c.Cluster.LocalID)
		c.DnsConf[i].Cluster.Group = c.DnsConf[i].ID
	}
}

func (c Config) Node(id string) (ClusterNode, bool) {
	for _, n := range c.Cluster.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	if id == c.Cluster.LocalID {
		name, _ := os.Hostname()
		if name == "" {
			name = id
		}
		return ClusterNode{ID: id, Name: name}, true
	}
	return ClusterNode{}, false
}

func (c Config) ValidateCluster() error {
	if c.Cluster.LocalID == "" || strings.ContainsAny(c.Cluster.LocalID, ",/ \t\r\n") {
		return errors.New("本机 ID 无效")
	}
	if len(c.Cluster.Nodes) > 128 {
		return errors.New("最多支持 128 个节点")
	}
	nodes := map[string]bool{}
	for _, n := range c.Cluster.Nodes {
		if n.ID == "" || strings.ContainsAny(n.ID, ",/ \t\r\n") || nodes[n.ID] {
			return errors.New("节点 ID 不能为空、重复或包含空白、逗号、斜杠")
		}
		nodes[n.ID] = true
		if n.ID != c.Cluster.LocalID || n.URL != "" {
			u, err := url.Parse(n.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("节点 %s 的访问地址无效", n.ID)
			}
		}
	}
	configIDs := map[string]bool{}
	for _, d := range c.DnsConf {
		if d.ID != "" {
			if configIDs[d.ID] {
				return errors.New("配置 ID 重复，请重新加载页面")
			}
			configIDs[d.ID] = true
		}
	}
	groups := map[string]bool{}
	for _, d := range c.DnsConf {
		p := d.Cluster
		if err := d.ValidateNetworks(c); err != nil {
			return err
		}
		if p.CooldownSeconds < 0 || p.CooldownSeconds > 3600 {
			return errors.New("前缀冷却时间须为 1–3600 秒，0 使用默认 60 秒")
		}
		if !p.Enabled {
			continue
		}
		if p.Group == "" || groups[p.Group] {
			return errors.New("启用集群的配置必须具有唯一配置组标识")
		}
		groups[p.Group] = true
		if p.PrefixLength < 1 || p.PrefixLength > 128 {
			return errors.New("IPv6 前缀长度必须为 1–128")
		}
		if p.Source != "node" && p.Source != "auto" && p.Source != "prefix" {
			return errors.New("未知 IP 来源")
		}
		if _, ok := clusterAlgorithms.Load(p.Algorithm); !ok {
			return errors.New("未知节点选择算法")
		}
		members := map[string]bool{}
		domains := map[string]bool{}
		legacyDomains := d.Ipv6.Domains
		if len(d.Networks) > 0 {
			legacyDomains = nil
		}
		for _, domain := range legacyDomains {
			if strings.TrimSpace(domain) == "" {
				continue
			}
			parsed := checkParseDomains([]string{domain})
			if len(parsed) != 1 {
				return errors.New("配置 IPv6 域名格式无效")
			}
			domains[strings.ToLower(strings.TrimSuffix(parsed[0].ToASCII(), "."))] = true
		}
		for _, id := range p.IDs() {
			n, ok := c.Node(id)
			if !ok || members[id] {
				return fmt.Errorf("节点 %s 不存在或重复", id)
			}
			members[id] = true
			if id != c.Cluster.LocalID && (len(n.Token) < 32 || len(c.Cluster.Token) < 32) {
				return errors.New("节点间通信需配置至少 32 字符的访问令牌和本机令牌")
			}
			if n.Suffix != "" {
				if err := validateSuffix(n.Suffix, p.PrefixLength); err != nil {
					return err
				}
			}
			for _, domain := range strings.Split(strings.ReplaceAll(n.IPv6Domains, "\r\n", "\n"), "\n") {
				if strings.TrimSpace(domain) == "" {
					continue
				}
				parsed := checkParseDomains([]string{domain})
				if len(parsed) != 1 {
					return fmt.Errorf("节点 %s 的 IPv6 域名格式无效", id)
				}
				key := strings.ToLower(strings.TrimSuffix(parsed[0].ToASCII(), "."))
				if domains[key] {
					return fmt.Errorf("节点的 IPv6 域名不能重复: %s", key)
				}
				domains[key] = true
			}
		}

		source := p.SourceNode
		if source == "local" {
			source = c.Cluster.LocalID
		}
		if p.Source != "auto" && p.SourceNode != "local" && !members[source] {
			return errors.New("IP 来源节点必须属于当前配置关联节点")
		}
		if p.Source == "prefix" {
			if err := validateSuffix(p.Suffix, p.PrefixLength); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSuffix(s string, bits int) error {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
		return errors.New("IPv6 后缀格式无效，例如 ::1234")
	}
	if netip.PrefixFrom(a, bits).Masked().Addr() != netip.IPv6Unspecified() {
		return errors.New("IPv6 后缀超出前缀长度允许的主机位")
	}
	return nil
}

// DisplayName keeps unnamed configurations identifiable in audit records.
func (d DnsConfig) DisplayName() string {
	if d.Name != "" {
		return d.Name
	}
	if d.Cluster.Group != "" {
		return d.Cluster.Group
	}
	return d.DNS.Name + " (" + d.ID + ")"
}
