package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jeessy2/ddns-go/v6/audit"
	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

var previousUpdates = map[string]string{} // guarded by updateMu; last successful local application

func runCluster(c config.Config, d config.DnsConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := d.Cluster
	if err := c.ValidateCluster(); err != nil {
		return err
	}
	path := util.GetConfigFilePath() + ".cluster-state.json"
	states, err := cluster.LoadState(path)
	if err != nil {
		return err
	}
	state := states[p.Group]
	if state == nil {
		state = &cluster.State{}
		states[p.Group] = state
	}
	layout := cluster.LayoutHash(c, d)
	if state.LayoutHash != layout {
		state.Prefix = ""
		state.Pending = nil
		state.PendingPrefix = ""
		state.LayoutHash = layout
	}
	if state.Addresses == nil {
		state.Addresses = map[string]string{}
	}
	if state.Reports == nil {
		state.Reports = map[string]string{}
	}
	alive := cluster.Alive(ctx, c, p)
	if len(alive) == 0 {
		return errors.New("所有关联节点均不可用，本次不更新 DNS")
	}
	execute := func(jobs []cluster.Job, batch bool) error {
		candidates := alive
		if batch {
			candidates = cluster.Alive(ctx, c, p)
		}
		executor, err := cluster.Select(p.Algorithm, p.Group+"/executor", candidates)
		if err != nil {
			return err
		}
		audit.Record(audit.Event{Actor: "scheduler", Host: c.Cluster.LocalID, Operation: "node.select", Config: d.DisplayName(), Node: executor.Name, After: executor.ID, Result: "selected"})
		req := cluster.UpdateRequest{Group: p.Group, Jobs: jobs, LayoutHash: layout}
		if executor.ID == c.Cluster.LocalID {
			return ExecuteCluster(c, d, req, c.Cluster.LocalID)
		}
		err = cluster.Execute(ctx, c, executor, req)
		result := "ok"
		if err != nil {
			result = "failed"
		}
		audit.Record(audit.Event{Actor: "scheduler", Host: c.Cluster.LocalID, Operation: "dns.dispatch", Config: d.DisplayName(), Node: executor.Name, Result: result})
		return err
	}
	// A persisted batch is retried before observing further prefix changes.
	if len(state.Pending) > 0 {
		if err := execute(state.Pending, true); err != nil {
			return err
		}
		for _, j := range state.Pending {
			state.Addresses[j.NodeID] = j.IPv6
		}
		state.Prefix = state.PendingPrefix
		state.Pending = nil
		state.PendingPrefix = ""
		if err := cluster.SaveState(path, states); err != nil {
			return err
		}
	}
	reports := map[string]config.ResolvedIP{}
	changed := map[string]bool{}
	for _, n := range alive {
		ip, err := cluster.ReadIP(ctx, c, d, n)
		if err != nil {
			continue
		}
		reports[n.ID] = ip
		if ip.IPv6 != "" {
			prefix, err := cluster.Prefix(ip.IPv6, p.PrefixLength)
			if err != nil {
				continue
			}
			old := state.Reports[n.ID]
			if state.Prefix == "" || (old != "" && old != ip.IPv6 && prefix != state.Prefix) {
				changed[prefix] = true
			}
			state.Reports[n.ID] = ip.IPv6
			if state.Addresses[n.ID] == "" {
				state.Addresses[n.ID] = ip.IPv6
			}
		}
	}
	if len(changed) > 1 {
		return errors.New("节点上报了多个不同的新 IPv6 前缀，请检查配置组是否属于同一网络")
	}
	sourceCandidates := []config.ClusterNode{}
	for _, n := range alive {
		if ip, ok := reports[n.ID]; ok {
			if d.Ipv4.Enable && hasDomains(d.Ipv4.Domains) && ip.IPv4 == "" {
				continue
			}
			if d.Ipv6.Enable && hasDomains(d.Ipv6.Domains) && ip.IPv6 == "" && (p.Source != "prefix" || state.Prefix == "") {
				continue
			}
			sourceCandidates = append(sourceCandidates, n)
		}
	}
	var source config.ClusterNode
	if p.Source == "auto" {
		source, err = cluster.Select(p.Algorithm, p.Group+"/source", sourceCandidates)
	} else {
		for _, n := range sourceCandidates {
			if n.ID == p.SourceNode {
				source = n
			}
		}
		if source.ID == "" {
			err = errors.New("指定 IP 来源节点不可用")
		}
	}
	// Prefix propagation is independent of the normal source node's availability.
	for prefix := range changed {
		if prefix == state.Prefix {
			continue
		}
		jobs := []cluster.Job{}
		for _, id := range p.IDs() {
			n, _ := c.Node(id)
			suffix := n.Suffix
			if suffix == "" {
				suffix = state.Addresses[id]
			}
			if strings.TrimSpace(n.IPv6Domains) == "" {
				continue
			}
			if suffix == "" {
				return fmt.Errorf("节点 %s 尚无 IPv6 后缀，请先配置后缀或等待其上报", id)
			}
			address, e := cluster.Compose(prefix, suffix)
			if e != nil {
				return e
			}
			jobs = append(jobs, cluster.Job{NodeID: id, IPv6: address, Batch: true})
		}
		audit.Record(audit.Event{Actor: "scheduler", Host: c.Cluster.LocalID, Operation: "ipv6.prefix", Config: d.DisplayName(), Before: state.Prefix, After: prefix, Result: "pending"})
		state.Pending = jobs
		state.PendingPrefix = prefix
		if e := cluster.SaveState(path, states); e != nil {
			return e
		}
		if len(jobs) > 0 {
			if e := execute(jobs, true); e != nil {
				return e
			}
		}
		for _, j := range jobs {
			state.Addresses[j.NodeID] = j.IPv6
		}
		state.Prefix = prefix
		state.Pending = nil
		state.PendingPrefix = ""
	}
	if e := cluster.SaveState(path, states); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	ip := reports[source.ID]
	if p.Source == "prefix" {
		if state.Prefix == "" {
			return errors.New("尚未获取 IPv6 前缀")
		}
		ip.IPv6, err = cluster.Compose(state.Prefix, p.Suffix)
		if err != nil {
			return err
		}
	}
	return execute([]cluster.Job{{NodeID: source.ID, IPv4: ip.IPv4, IPv6: ip.IPv6}}, false)
}

// ExecuteCluster uses local provider credentials and local domain bindings only.
// It never recursively schedules or accepts executable settings from a peer.
func ExecuteCluster(c config.Config, d config.DnsConfig, r cluster.UpdateRequest, actor string) error {
	if !d.Cluster.Enabled || r.Group != d.Cluster.Group || actor != d.Cluster.Coordinator {
		return errors.New("配置组或调度节点不匹配")
	}
	if r.LayoutHash != cluster.LayoutHash(c, d) {
		return errors.New("节点配置组设置不一致，请同步组成员、域名与 IP 来源设置")
	}
	if len(r.Jobs) == 0 || len(r.Jobs) > len(d.Cluster.IDs())+1 {
		return errors.New("无效更新批次")
	}
	members := map[string]bool{}
	for _, id := range d.Cluster.IDs() {
		members[id] = true
	}
	for _, j := range r.Jobs {
		if !members[j.NodeID] {
			return errors.New("更新目标不属于配置组")
		}
		if err := cluster.ValidateIP(config.ResolvedIP{IPv4: j.IPv4, IPv6: j.IPv6}); err != nil {
			return err
		}
		if j.Batch && (j.IPv6 == "" || j.IPv4 != "") {
			return errors.New("无效 IPv6 批量任务")
		}
	}
	updateMu.Lock()
	defer updateMu.Unlock()
	for _, j := range r.Jobs {
		dc := d
		n, _ := c.Node(j.NodeID)
		if j.Batch {
			dc.Ipv4.Enable = false
			dc.Ipv4.Domains = nil
			dc.Ipv6.Domains = util.SplitLines(n.IPv6Domains)
		}
		if j.Batch && (!dc.Ipv6.Enable || strings.TrimSpace(n.IPv6Domains) == "") {
			return errors.New("节点未配置 IPv6 域名或 IPv6 未启用")
		}
		if dc.Ipv4.Enable && hasDomains(dc.Ipv4.Domains) && j.IPv4 == "" {
			return errors.New("缺少 IPv4 地址")
		}
		if dc.Ipv6.Enable && hasDomains(dc.Ipv6.Domains) && j.IPv6 == "" {
			return errors.New("缺少 IPv6 地址")
		}
		dc.Resolved = &config.ResolvedIP{IPv4: j.IPv4, IPv6: j.IPv6}
		caches := [2]util.IpCache{}
		provider := newProvider(dc.DNS.Name)
		provider.Init(&dc, &caches[0], &caches[1])
		domains := provider.AddUpdateDomainRecords()
		v4, v6 := config.ExecWebhook(&domains, &c)
		result := "ok"
		if v4 == config.UpdatedFailed || v6 == config.UpdatedFailed {
			result = "failed"
		}
		after, _ := json.Marshal(struct {
			IPv4        string
			IPv6        string
			IPv4Domains []string
			IPv6Domains []string
		}{j.IPv4, j.IPv6, dc.Ipv4.Domains, dc.Ipv6.Domains})
		audit.Record(audit.Event{Actor: actor, Host: c.Cluster.LocalID, Operation: "dns.update", Config: d.DisplayName(), Node: n.Name, Before: previousValue(d.Cluster.Group + "/" + j.NodeID + fmt.Sprint(j.Batch)), After: string(after), Result: result})
		if result == "ok" {
			previousUpdates[d.Cluster.Group+"/"+j.NodeID+fmt.Sprint(j.Batch)] = string(after)
		}
		if result != "ok" {
			return errors.New("DNS 服务商更新失败，稍后重试")
		}
	}
	return nil
}

func previousValue(key string) string {
	if s := previousUpdates[key]; s != "" {
		return s
	}
	return "unknown"
}

func hasDomains(domains []string) bool {
	for _, d := range domains {
		if strings.TrimSpace(d) != "" {
			return true
		}
	}
	return false
}
