package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// IPConfig keeps the original URL/interface/command acquisition contract.
type IPConfig struct {
	Enable                                   bool
	GetType, URL, NetInterface, Cmd, Ipv6Reg string
	Domains                                  []string
}
type NodeNetwork struct {
	NodeID     string
	Ipv4, Ipv6 IPConfig
	Suffixes   string // optional IPv6 suffixes, one per domain
}

func (d DnsConfig) ForNode(id string) DnsConfig {
	if len(d.Networks) == 0 {
		return d
	}
	d.Ipv4 = IPConfig{}
	d.Ipv6 = IPConfig{}
	for _, n := range d.Networks {
		if n.NodeID == id {
			d.Ipv4 = n.Ipv4
			d.Ipv6 = n.Ipv6
			break
		}
	}
	return d
}
func (d DnsConfig) Network(id string) (NodeNetwork, bool) {
	for _, n := range d.Networks {
		if n.NodeID == id {
			return n, true
		}
	}
	return NodeNetwork{}, false
}
func (d DnsConfig) ValidateNetworks(c Config) error {
	seen := map[string]bool{}
	domains := map[string]bool{}
	for _, raw := range d.Networks {
		n, _ := d.NetworkWithSuffix(raw.NodeID)
		if _, ok := c.Node(n.NodeID); !ok || seen[n.NodeID] {
			return fmt.Errorf("节点 %s 不存在或网络配置重复", n.NodeID)
		}
		seen[n.NodeID] = true
		for family, ip := range []IPConfig{n.Ipv4, n.Ipv6} {
			if ip.GetType != "" && ip.GetType != "url" && ip.GetType != "netInterface" && ip.GetType != "cmd" {
				return fmt.Errorf("节点 %s 的 IP 获取方式无效", n.NodeID)
			}
			if ip.Ipv6Reg != "" {
				if _, err := regexp.Compile(ip.Ipv6Reg); err != nil {
					return fmt.Errorf("节点 %s 的匹配正则无效: %w", n.NodeID, err)
				}
			}
			for _, domain := range ip.Domains {
				if strings.TrimSpace(domain) == "" {
					continue
				}
				parsed := checkParseDomains([]string{domain})
				if len(parsed) != 1 {
					return fmt.Errorf("节点 %s 的域名无效", n.NodeID)
				}
				key := fmt.Sprint(family) + strings.ToLower(strings.TrimSuffix(parsed[0].ToASCII(), "."))
				if domains[key] {
					return fmt.Errorf("同一配置中同类型域名不能属于多个节点: %s", domain)
				}
				domains[key] = true
			}
		}
		if strings.TrimSpace(n.Suffixes) != "" {
			suffixes := strings.Split(strings.ReplaceAll(n.Suffixes, "\r\n", "\n"), "\n")
			for len(suffixes) > len(n.Ipv6.Domains) && strings.TrimSpace(suffixes[len(suffixes)-1]) == "" {
				suffixes = suffixes[:len(suffixes)-1]
			}
			if len(suffixes) > len(n.Ipv6.Domains) {
				return fmt.Errorf("节点 %s 的 IPv6 后缀须与域名逐行对应", n.NodeID)
			}
			for _, suffix := range suffixes {
				if strings.TrimSpace(suffix) != "" {
					if err := validateSuffix(strings.TrimSpace(suffix), d.Cluster.PrefixLength); err != nil {
						return err
					}
				}
			}
		}
	}
	if d.IPv6Suffix != nil && d.IPv6Suffix.Enable {
		lines := strings.Split(strings.TrimSpace(d.IPv6Suffix.IPs), "\n")
		if len(d.IPv6Suffix.Domains) == 0 || len(lines) != len(d.IPv6Suffix.Domains) {
			return fmt.Errorf("自定义 IPv6 后缀和 Domains 必须逐行对应，且不能为空")
		}
		for i, domain := range d.IPv6Suffix.Domains {
			if err := validateSuffix(strings.TrimSpace(lines[i]), d.Cluster.PrefixLength); err != nil {
				return err
			}
			parsed := checkParseDomains([]string{domain})
			if len(parsed) != 1 {
				return fmt.Errorf("自定义后缀域名无效")
			}
			key := "1" + strings.ToLower(strings.TrimSuffix(parsed[0].ToASCII(), "."))
			if domains[key] {
				return fmt.Errorf("自定义后缀域名重复或与节点 IPv6 域名冲突: %s", domain)
			}
			domains[key] = true
		}
	}
	if d.Cluster.Enabled && len(d.Networks) > 0 {
		for _, id := range d.Cluster.IDs() {
			if !seen[id] {
				return fmt.Errorf("请补充节点 %s 的网络配置", id)
			}
		}
	}
	return nil
}

// Quoted suffix strings preserve blank-line/domain alignment. yaml.v3's literal
// block indentation can otherwise produce unreadable nested leading-newline values.
func (n NodeNetwork) MarshalYAML() (any, error) {
	return struct {
		NodeID     string
		Ipv4, Ipv6 IPConfig
		Suffixes   *yaml.Node
	}{n.NodeID, n.Ipv4, n.Ipv6, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: n.Suffixes, Style: yaml.DoubleQuotedStyle}}, nil
}

// Each DNS configuration has one optional suffix list with its own domain bindings.
type SuffixConfig struct {
	Enable  bool
	IPs     string
	Domains []string
}

func (s SuffixConfig) MarshalYAML() (any, error) {
	return struct {
		Enable  bool
		IPs     *yaml.Node
		Domains []string
	}{s.Enable, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.IPs, Style: yaml.DoubleQuotedStyle}, s.Domains}, nil
}
func (d DnsConfig) NetworkWithSuffix(id string) (NodeNetwork, bool) {
	n, ok := d.Network(id)
	if d.IPv6Suffix != nil {
		n.Suffixes = ""
	}
	return n, ok
}
