package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
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
	alive := cluster.Alive(ctx, c, p)
	lease, err := cluster.Acquire(ctx, c, alive, "dns/"+p.Group)
	if errors.Is(err, cluster.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lease.Release()
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
	layoutChanged := state.LayoutHash != layout
	if layoutChanged {
		oldPrefix, e := netip.ParsePrefix(state.Prefix)
		if e != nil || oldPrefix.Bits() != p.PrefixLength {
			state.Prefix = ""
			state.PrefixAt = time.Time{}
			state.Retired = nil
		}
		state.LastKey = ""
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
	// Read the newest completed or pending state while holding the execution claim.
	for _, n := range alive {
		if n.ID == c.Cluster.LocalID {
			continue
		}
		var remote cluster.State
		if cluster.Call(ctx, n, c.Cluster.LocalID, "GET", "/cluster/state?group="+url.QueryEscape(p.Group), nil, &remote) == nil && remote.Version.Newer(state.Version) && remote.LayoutHash == layout {
			state = &remote
			layoutChanged = false
		}
	}
	if state.Addresses == nil {
		state.Addresses = map[string]string{}
	}
	if state.Reports == nil {
		state.Reports = map[string]string{}
	}
	saved := false
	persist := func() error {
		state.Version.Counter++
		state.Version.Node = c.Cluster.LocalID
		err := cluster.SaveGroup(path, p.Group, state)
		if err == nil {
			saved = true
		}
		return err
	}
	defer func() {
		if !saved {
			return
		}
		for _, n := range alive {
			if n.ID == c.Cluster.LocalID {
				continue
			}
			var result any
			push, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := cluster.Call(push, n, c.Cluster.LocalID, "POST", "/cluster/state", struct {
				Group  string
				Ticket string
				State  *cluster.State
			}{p.Group, lease.Claim.Ticket, state}, &result)
			cancel()
			if err != nil {
				util.Log("节点 %s 的运行状态将在下次任务时重试同步", n.ID)
			}
		}
	}()
	execute := func(jobs []cluster.Job, batch bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !lease.Valid() {
			return errors.New("本机执行权已过期")
		}
		if batch {
			cluster.Alive(ctx, c, p)
		} // actively probe again immediately before a prefix batch
		audit.Record(audit.Event{Actor: "scheduler", Host: c.Cluster.LocalID, Operation: "node.select", Config: d.DisplayName(), Node: c.Cluster.LocalID, After: c.Cluster.LocalID, Result: "local execution"})
		return executeCluster(ctx, c, d, cluster.UpdateRequest{Group: p.Group, Jobs: jobs, LayoutHash: layout}, c.Cluster.LocalID)
	}

	prefixUpdated := false
	// A persisted batch is retried before observing further prefix changes.
	if len(state.Pending) > 0 {
		if err := execute(state.Pending, true); err != nil {
			return err
		}
		prefixUpdated = true
		state.CommitPrefix(state.PendingPrefix, p.CooldownSeconds, time.Now().UTC())
		state.Pending = nil
		state.PendingPrefix = ""
		if err := persist(); err != nil {
			return err
		}
	}
	reports := map[string]config.ResolvedIP{}
	for _, n := range alive {
		ip, e := cluster.ReadIP(ctx, c, d, n)
		if e != nil {
			continue
		}
		reports[n.ID] = ip
		if ip.IPv6 != "" {
			state.Reports[n.ID] = ip.IPv6
			if state.Addresses[n.ID] == "" {
				state.Addresses[n.ID] = ip.IPv6
			}
		}
	}
	prefix, err := state.CandidatePrefix(reports, p.PrefixLength, time.Now().UTC())
	if err != nil {
		return err
	}
	if prefix != "" && (prefix != state.Prefix || layoutChanged) {
		jobs, e := prefixJobs(c, d, state, prefix)
		if e != nil {
			return e
		}
		state.Pending = jobs
		state.PendingPrefix = prefix
		audit.Record(audit.Event{Actor: "scheduler", Host: c.Cluster.LocalID, Operation: "ipv6.prefix", Config: d.DisplayName(), Before: state.Prefix, After: prefix, Result: "pending"})
		if e := persist(); e != nil {
			return e
		}
		if len(jobs) > 0 {
			if e := execute(jobs, true); e != nil {
				return e
			}
		}
		prefixUpdated = true
		state.CommitPrefix(prefix, p.CooldownSeconds, time.Now().UTC())
		state.Pending = nil
		state.PendingPrefix = ""
	}
	if err := persist(); err != nil {
		return err
	}
	jobs, err := normalJobs(c, d, state, reports, alive)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	keyBytes, _ := json.Marshal(struct {
		Layout  string
		Version config.SyncVersion
		Jobs    []cluster.Job
	}{layout, c.Cluster.Version, jobs})
	key := string(keyBytes)
	if state.LastKey == key && time.Since(state.LastAt) >= 0 && time.Since(state.LastAt) < time.Minute {
		return nil
	}
	executeJobs := jobs
	if prefixUpdated && len(d.Networks) > 0 {
		executeJobs = nil
		for _, job := range jobs {
			if job.IPv4 != "" {
				executeJobs = append(executeJobs, job)
			}
		}
	}
	if len(executeJobs) > 0 {
		if err := execute(executeJobs, false); err != nil {
			return err
		}
	}
	state.LastKey = key
	state.LastAt = time.Now().UTC()
	return persist()
}

func prefixJobs(c config.Config, d config.DnsConfig, s *cluster.State, prefix string) ([]cluster.Job, error) {
	var jobs []cluster.Job
	for _, id := range d.Cluster.IDs() {
		n, _ := c.Node(id)
		if len(d.Networks) > 0 {
			network, ok := d.NetworkWithSuffix(id)
			if !ok || !network.Ipv6.Enable || !hasDomains(network.Ipv6.Domains) {
				continue
			}
			ip := s.Addresses[id]
			if ip == "" && strings.TrimSpace(network.Suffixes) == "" {
				return nil, fmt.Errorf("节点 %s 尚无可用 IPv6 后缀", id)
			}
			next, err := networkJobs(network, config.ResolvedIP{IPv6: ip}, prefix, true)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, next...)
		} else {
			suffix := n.Suffix
			if suffix == "" {
				suffix = s.Addresses[id]
			}
			if strings.TrimSpace(n.IPv6Domains) == "" {
				continue
			}
			if suffix == "" {
				return nil, fmt.Errorf("节点 %s 尚无 IPv6 后缀", id)
			}
			address, err := cluster.Compose(prefix, suffix)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, cluster.Job{NodeID: id, IPv6: address, Batch: true})
		}
	}
	extra, err := customSuffixJobs(c, d, prefix, true)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, extra...)
	return jobs, nil
}

// Custom suffix domains are independent of every node's own domain bindings.
func customSuffixJobs(c config.Config, d config.DnsConfig, prefix string, batch bool) ([]cluster.Job, error) {
	if d.IPv6Suffix == nil || !d.IPv6Suffix.Enable {
		return nil, nil
	}
	if prefix == "" {
		return nil, errors.New("自定义后缀尚无可用 IPv6 前缀，请启用至少一个节点的 IPv6 获取")
	}
	lines := strings.Split(strings.TrimSpace(d.IPv6Suffix.IPs), "\n")
	if len(lines) != len(d.IPv6Suffix.Domains) || len(d.IPv6Suffix.Domains) == 0 {
		return nil, errors.New("自定义后缀和域名数量不一致")
	}
	var jobs []cluster.Job
	for i, suffix := range lines {
		ip, err := cluster.Compose(prefix, strings.TrimSpace(suffix))
		if err != nil {
			return nil, err
		}
		idx := i
		jobs = append(jobs, cluster.Job{NodeID: c.Cluster.LocalID, IPv6: ip, Batch: batch, CustomSuffix: true, IPv6Index: &idx})
	}
	return jobs, nil
}

func networkJobs(n config.NodeNetwork, ip config.ResolvedIP, prefix string, batch bool) ([]cluster.Job, error) {
	var jobs []cluster.Job
	if n.Ipv4.Enable && !batch && hasDomains(n.Ipv4.Domains) && ip.IPv4 != "" {
		jobs = append(jobs, cluster.Job{NodeID: n.NodeID, IPv4: ip.IPv4})
	}
	if !n.Ipv6.Enable || !hasDomains(n.Ipv6.Domains) {
		return jobs, nil
	}
	suffixes := strings.Split(strings.ReplaceAll(n.Suffixes, "\r\n", "\n"), "\n")
	for i := range n.Ipv6.Domains {
		address := ip.IPv6
		if strings.TrimSpace(n.Suffixes) != "" && i < len(suffixes) && strings.TrimSpace(suffixes[i]) != "" {
			address = strings.TrimSpace(suffixes[i])
		}
		if address == "" {
			if batch {
				return nil, fmt.Errorf("节点 %s 尚无可用 IPv6 后缀", n.NodeID)
			}
			continue
		}
		if prefix != "" {
			var err error
			address, err = cluster.Compose(prefix, address)
			if err != nil {
				return nil, err
			}
		}
		jobs = append(jobs, cluster.Job{NodeID: n.NodeID, IPv6: address, Batch: batch, IPv6Index: new(int)})
		*jobs[len(jobs)-1].IPv6Index = i
	}
	return jobs, nil
}
func normalJobs(c config.Config, d config.DnsConfig, s *cluster.State, reports map[string]config.ResolvedIP, alive []config.ClusterNode) ([]cluster.Job, error) {
	if len(d.Networks) > 0 {
		var jobs []cluster.Job
		for _, raw := range d.Networks {
			n, _ := d.NetworkWithSuffix(raw.NodeID)
			ip, ok := reports[n.NodeID]
			if !ok {
				continue
			}
			next, err := networkJobs(n, ip, s.Prefix, false)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, next...)
		}
		extra, err := customSuffixJobs(c, d, s.Prefix, false)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, extra...)
		return jobs, nil
	}
	// Compatibility with previously saved source-node policies.
	var candidates []config.ClusterNode
	for _, n := range alive {
		ip, ok := reports[n.ID]
		if !ok {
			continue
		}
		if d.Ipv4.Enable && hasDomains(d.Ipv4.Domains) && ip.IPv4 == "" {
			continue
		}
		if d.Ipv6.Enable && hasDomains(d.Ipv6.Domains) && ip.IPv6 == "" {
			continue
		}
		candidates = append(candidates, n)
	}
	var source config.ClusterNode
	var err error
	if d.Cluster.Source == "auto" {
		source, err = cluster.Select(d.Cluster.Algorithm, d.Cluster.Group+"/source", candidates)
	} else {
		for _, n := range candidates {
			if n.ID == d.Cluster.SourceNode || (d.Cluster.SourceNode == "local" && n.ID == c.Cluster.LocalID) {
				source = n
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if source.ID == "" {
		return nil, errors.New("指定 IP 来源节点不可用")
	}
	ip := reports[source.ID]
	if ip.IPv6 != "" && s.Prefix != "" {
		suffix := ip.IPv6
		if d.Cluster.Source == "prefix" {
			suffix = d.Cluster.Suffix
		}
		ip.IPv6, err = cluster.Compose(s.Prefix, suffix)
		if err != nil {
			return nil, err
		}
	}
	return []cluster.Job{{NodeID: source.ID, IPv4: ip.IPv4, IPv6: ip.IPv6}}, nil
}

// ExecuteCluster uses local provider credentials and local domain bindings only.
// Every node executes locally after acquiring a short-lived peer claim.
func ExecuteCluster(c config.Config, d config.DnsConfig, r cluster.UpdateRequest, actor string) error {
	return executeCluster(context.Background(), c, d, r, actor)
}
func executeCluster(ctx context.Context, c config.Config, d config.DnsConfig, r cluster.UpdateRequest, actor string) error {
	if !d.Cluster.Enabled || r.Group != d.Cluster.Group || actor != c.Cluster.LocalID || !cluster.IsMember(d.Cluster, actor) {
		return errors.New("配置组或本机执行身份不匹配")
	}
	if r.LayoutHash != cluster.LayoutHash(c, d) {
		return errors.New("节点配置组设置不一致，请同步组成员、域名与 IP 来源设置")
	}
	if len(r.Jobs) == 0 || len(r.Jobs) > 4096 {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		dc := d.ForNode(j.NodeID)
		if j.CustomSuffix {
			if d.IPv6Suffix == nil || !d.IPv6Suffix.Enable || j.IPv6Index == nil || j.IPv4 != "" || j.IPv6 == "" {
				return errors.New("无效自定义后缀更新任务")
			}
			dc.Ipv4 = config.IPConfig{}
			dc.Ipv6 = config.IPConfig{Enable: true, Domains: d.IPv6Suffix.Domains}
		}
		dc.RequestContext = ctx
		n, _ := c.Node(j.NodeID)
		if j.Batch && !j.CustomSuffix && len(d.Networks) == 0 {
			dc.Ipv4.Enable = false
			dc.Ipv4.Domains = nil
			dc.Ipv6.Domains = util.SplitLines(n.IPv6Domains)
		}
		if j.Batch && !j.CustomSuffix && len(d.Networks) == 0 && (!dc.Ipv6.Enable || strings.TrimSpace(n.IPv6Domains) == "") {
			return errors.New("节点未配置 IPv6 域名或 IPv6 未启用")
		}
		if len(d.Networks) > 0 || j.CustomSuffix {
			if j.IPv4 == "" {
				dc.Ipv4.Enable = false
				dc.Ipv4.Domains = nil
			}
			if j.IPv6 == "" {
				dc.Ipv6.Enable = false
				dc.Ipv6.Domains = nil
			}
			if j.IPv6Index != nil {
				idx := *j.IPv6Index
				if idx < 0 || idx >= len(dc.Ipv6.Domains) {
					return errors.New("域名索引无效")
				}
				dc.Ipv6.Domains = []string{dc.Ipv6.Domains[idx]}
			}
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
		audit.Record(audit.Event{Actor: actor, Host: c.Cluster.LocalID, Operation: "dns.update", Config: d.DisplayName(), Node: n.Name, Before: previousValue(d.Cluster.Group + "/" + j.NodeID + fmt.Sprint(j.Batch) + fmt.Sprint(j.CustomSuffix) + fmt.Sprint(j.IPv6IndexValue())), After: string(after), Result: result})
		if result == "ok" {
			previousUpdates[d.Cluster.Group+"/"+j.NodeID+fmt.Sprint(j.Batch)+fmt.Sprint(j.CustomSuffix)+fmt.Sprint(j.IPv6IndexValue())] = string(after)
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
