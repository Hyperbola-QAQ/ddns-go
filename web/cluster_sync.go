package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/audit"
	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/dns"
)

// SharedSnapshot contains the complete editable configuration. LocalID and inbound
// token identify this process; peer network settings and credentials are shared.
type SharedSnapshot struct {
	Version           config.SyncVersion
	Actor             string
	Nodes             []config.ClusterNode
	Configs           []config.DnsConfig
	User              config.User
	Webhook           config.Webhook
	Audit             config.AuditSettings
	NotAllowWanAccess bool
	Lang              string
}

func sharedSnapshot(c config.Config) SharedSnapshot {
	s := SharedSnapshot{Version: c.Cluster.Version, Actor: c.Cluster.Version.Actor, Nodes: cluster.Members(c), Configs: c.DnsConf, User: c.User, Webhook: c.Webhook, Audit: c.Audit, NotAllowWanAccess: c.NotAllowWanAccess, Lang: c.Lang}
	for i := range s.Nodes {
		if s.Nodes[i].ID == c.Cluster.LocalID {
			s.Nodes[i].Token = c.Cluster.Token
		}
	}
	return s
}
func bumpClusterVersion(c *config.Config) {
	c.Cluster.Version.Counter++
	c.Cluster.Version.Node = c.Cluster.LocalID
	c.Cluster.Version.Actor = c.Username
}
func mergeSnapshot(c config.Config, s SharedSnapshot) (config.Config, error) {
	if !s.Version.Newer(c.Cluster.Version) {
		return c, nil
	}
	nodes := append([]config.ClusterNode(nil), s.Nodes...)
	found := false
	for i := range nodes {
		if old, ok := c.Node(nodes[i].ID); ok && nodes[i].URL == "" {
			nodes[i].URL = old.URL
		}
		if nodes[i].ID == c.Cluster.LocalID {
			nodes[i].Token = c.Cluster.Token
			found = true
		}
	}
	if !found {
		return c, errors.New("同步拓扑必须包含本机节点")
	}
	c.DnsConf = append([]config.DnsConfig(nil), s.Configs...)
	c.User = s.User
	c.Webhook = s.Webhook
	c.Audit = s.Audit
	c.NotAllowWanAccess = s.NotAllowWanAccess
	c.Lang = s.Lang
	c.Cluster.Nodes = nodes
	c.Cluster.Version = s.Version
	c.ClusterDefaults()
	return c, c.ValidateCluster()
}
func acceptSnapshot(s SharedSnapshot, host string) error {
	config.EditMu.Lock()
	defer config.EditMu.Unlock()
	return acceptSnapshotLocked(s, host)
}

func acceptSnapshotLocked(s SharedSnapshot, host string) error {
	c, err := config.GetConfigCached()
	if err != nil {
		return err
	}
	if !s.Version.Newer(c.Cluster.Version) {
		return nil
	}
	next, err := mergeSnapshot(c, s)
	if err != nil {
		return err
	}
	if err = next.SaveConfig(); err != nil {
		return err
	}
	if c.User != next.User {
		invalidateSessions()
		audit.Record(audit.Event{Actor: s.Actor, Host: s.Version.Node, Operation: "account.sync", Node: next.Cluster.LocalID, Before: c.Username + " [credentials redacted]", After: next.Username + " [credentials redacted]", Result: "ok"})
	}
	if !reflect.DeepEqual(c.Cluster.Nodes, next.Cluster.Nodes) {
		oldNodes := append([]config.ClusterNode(nil), c.Cluster.Nodes...)
		newNodes := append([]config.ClusterNode(nil), next.Cluster.Nodes...)
		for i := range oldNodes {
			oldNodes[i] = redactNode(oldNodes[i])
		}
		for i := range newNodes {
			newNodes[i] = redactNode(newNodes[i])
		}
		b, _ := json.Marshal(oldNodes)
		a, _ := json.Marshal(newNodes)
		audit.Record(audit.Event{Actor: s.Actor, Host: s.Version.Node, Operation: "node.sync", Node: next.Cluster.LocalID, Before: string(b), After: string(a), Result: "received from " + host})
	}
	before := map[string]config.DnsConfig{}
	for _, d := range c.DnsConf {
		{
			before[d.ID] = d
		}
	}
	for _, d := range next.DnsConf {
		{
			prev := before[d.ID]
			delete(before, d.ID)
			b, _ := json.Marshal(redactDNS(prev))
			a, _ := json.Marshal(redactDNS(d))
			audit.Record(audit.Event{Actor: s.Actor, Host: s.Version.Node, Operation: "config.sync", Config: d.DisplayName(), Node: next.Cluster.LocalID, Before: string(b), After: string(a), Result: "ok"})
		}
	}
	for _, d := range before {
		b, _ := json.Marshal(redactDNS(d))
		audit.Record(audit.Event{Actor: s.Actor, Host: s.Version.Node, Operation: "config.sync.delete", Config: d.DisplayName(), Node: next.Cluster.LocalID, Before: string(b), After: "null", Result: "ok"})
	}
	return nil
}

// Initial enrollment is authenticated with the receiving node's token. Ordinary
// synchronization still requires a known peer and uses logical version ordering.
func ClusterJoin(w http.ResponseWriter, r *http.Request) {
	if !jsonMutation(w, r) {
		return
	}
	var s SharedSnapshot
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&s) != nil {
		http.Error(w, "invalid snapshot", 400)
		return
	}
	config.EditMu.Lock()
	defer config.EditMu.Unlock()
	c, _ := config.GetConfigCached()
	sender := r.Header.Get("X-DDNS-Node")
	if _, known := c.Node(sender); known || sender == "" || s.Version.Node != sender {
		http.Error(w, "节点已加入或发送节点无效", 409)
		return
	}
	found := false
	for _, n := range s.Nodes {
		if n.ID == sender && n.URL != "" && len(n.Token) >= 32 {
			found = true
		}
	}
	if !found {
		http.Error(w, "请填写发起节点的访问地址和令牌", 400)
		return
	}
	// Joining explicitly adopts the initiating node's configuration, even if this
	// standalone receiver previously performed more local edits.
	if s.Version.Counter <= c.Cluster.Version.Counter {
		s.Version.Counter = c.Cluster.Version.Counter + 1
	}
	if err := acceptSnapshotLocked(s, sender); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	next, _ := config.GetConfigCached()
	jsonResponse(w, sharedSnapshot(next))
}
func ClusterSync(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		c, _ := config.GetConfigCached()
		jsonResponse(w, sharedSnapshot(c))
		return
	}
	if !jsonMutation(w, r) {
		return
	}
	var s SharedSnapshot
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&s) != nil {
		http.Error(w, "invalid snapshot", 400)
		return
	}
	if err := acceptSnapshot(s, r.Header.Get("X-DDNS-Node")); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

var syncWake = make(chan struct{}, 1)
var syncStatus struct {
	sync.Mutex
	Errors               map[string]string
	Requested, Completed uint64
}

func queueClusterSync() {
	syncStatus.Lock()
	syncStatus.Requested++
	syncStatus.Unlock()
	select {
	case syncWake <- struct{}{}:
	default:
	}
}
func clusterSyncPending() bool {
	syncStatus.Lock()
	defer syncStatus.Unlock()
	return syncStatus.Requested != syncStatus.Completed
}
func clusterSyncErrors() map[string]string {
	syncStatus.Lock()
	defer syncStatus.Unlock()
	out := map[string]string{}
	for k, v := range syncStatus.Errors {
		out[k] = v
	}
	return out
}

// Save-triggered pushes are immediate; periodic anti-entropy repairs offline peers and restarts.
func RunClusterSync() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-syncWake:
			syncClusterOnce()
			dns.RequestRun()
			continue
		case <-ticker.C:
		}
		syncClusterOnce()
	}
}
func syncClusterOnce() {
	syncStatus.Lock()
	generation := syncStatus.Requested
	syncStatus.Unlock()
	c, err := config.GetConfigCached()
	if err != nil {
		return
	}
	results := map[string]string{}
	for _, n := range c.Cluster.Nodes {
		if n.ID == c.Cluster.LocalID {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		var remote SharedSnapshot
		err := cluster.Call(ctx, n, c.Cluster.LocalID, "GET", "/cluster/sync", nil, &remote)
		if err != nil {
			// A knows B after saving, but B may not know A yet.
			err = cluster.Call(ctx, n, c.Cluster.LocalID, "POST", "/cluster/join", sharedSnapshot(c), &remote)
		}
		if err == nil && remote.Version.Newer(c.Cluster.Version) {
			err = acceptSnapshot(remote, n.ID)
			if err == nil {
				c, _ = config.GetConfigCached()
			}
		}
		if err == nil && c.Cluster.Version.Newer(remote.Version) {
			var out any
			err = cluster.Call(ctx, n, c.Cluster.LocalID, "POST", "/cluster/sync", sharedSnapshot(c), &out)
		}
		cancel()
		if err != nil {
			results[n.ID] = "同步失败，将自动重试"
		}
	}
	syncStatus.Lock()
	syncStatus.Errors = results
	syncStatus.Completed = generation
	syncStatus.Unlock()
}
