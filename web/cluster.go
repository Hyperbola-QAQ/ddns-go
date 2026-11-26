package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jeessy2/ddns-go/v6/audit"
	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

func Ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ok"}`)
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func jsonMutation(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "JSON required", 415)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "invalid origin", 403)
			return false
		}
	}
	return true
}

// Peer authentication is separate from session cookies; only ping is public.
func PeerAuth(f ViewFunc) ViewFunc {
	return PeerTokenAuth(func(w http.ResponseWriter, r *http.Request) {
		c, _ := config.GetConfigCached()
		if _, ok := c.Node(r.Header.Get("X-DDNS-Node")); !ok {
			http.Error(w, "unknown node", 403)
			return
		}
		f(w, r)
	})
}

// Possession of the target's token permits initial enrollment, before peers know each other.
func PeerTokenAuth(f ViewFunc) ViewFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := config.GetConfigCached()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || len(c.Cluster.Token) < 32 || subtle.ConstantTimeCompare([]byte(token), []byte(c.Cluster.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		f(w, r)
	}
}

func GenerateClusterToken(w http.ResponseWriter, r *http.Request) {
	if !jsonMutation(w, r) {
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "生成令牌失败", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]string{"Token": hex.EncodeToString(b)})
}

func ClusterIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	c, _ := config.GetConfigCached()
	n, _ := c.Node(c.Cluster.LocalID)
	n.Token = ""
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, n)
}
func groupConfig(c config.Config, group string) (config.DnsConfig, bool) {
	for _, d := range c.DnsConf {
		if d.Cluster.Enabled && d.Cluster.Group == group {
			return d, true
		}
	}
	return config.DnsConfig{}, false
}
func ClusterIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	c, _ := config.GetConfigCached()
	d, ok := groupConfig(c, r.URL.Query().Get("group"))
	if !ok || !cluster.IsMember(d.Cluster, r.Header.Get("X-DDNS-Node")) {
		http.Error(w, "group not available", 403)
		return
	}
	if layout := r.URL.Query().Get("layout"); layout != "" && layout != cluster.LayoutHash(c, d) {
		http.Error(w, "配置同步尚未完成", 409)
		return
	}
	n, _ := c.Node(c.Cluster.LocalID)
	ip, err := cluster.ReadIP(r.Context(), c, d, n)
	if err != nil {
		http.Error(w, "IP unavailable", 503)
		return
	}
	jsonResponse(w, ip)
}

type clusterPolicyView struct {
	ID, Name string
	Cluster  config.ClusterPolicy
}

func policyViews(c config.Config) []clusterPolicyView {
	var out []clusterPolicyView
	for _, d := range c.DnsConf {
		out = append(out, clusterPolicyView{d.ID, d.DisplayName(), d.Cluster})
	}
	return out
}

func ClusterSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		c, _ := config.GetConfigCached()
		c.ClusterDefaults()
		c.Cluster.Nodes = cluster.Members(c)
		for i := range c.Cluster.Nodes {
			if c.Cluster.Nodes[i].ID == c.Cluster.LocalID {
				c.Cluster.Nodes[i].Token = c.Cluster.Token
			}
		}
		jsonResponse(w, struct {
			Cluster         config.ClusterSettings
			Audit           config.AuditSettings
			Policies        []clusterPolicyView
			SyncErrors      map[string]string
			SyncPending     bool
			DefaultNodeName string
		}{c.Cluster, c.Audit, policyViews(c), clusterSyncErrors(), clusterSyncPending(), c.DefaultNodeName()})
		return
	}
	if !jsonMutation(w, r) {
		return
	}
	config.EditMu.Lock()
	defer config.EditMu.Unlock()
	var in struct {
		Cluster  config.ClusterSettings
		Audit    config.AuditSettings
		Policies []clusterPolicyView
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	c, _ := config.GetConfigCached()
	old := c
	if in.Cluster.LocalID != c.Cluster.LocalID {
		http.Error(w, "本机 ID 由系统自动生成，不允许修改", 400)
		return
	}
	if in.Cluster.Token == "" {
		in.Cluster.Token = c.Cluster.Token
	}
	if in.Cluster.Token == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			http.Error(w, "生成令牌失败", 500)
			return
		}
		in.Cluster.Token = hex.EncodeToString(b)
	}
	if len(in.Cluster.Token) < 32 {
		http.Error(w, "本机令牌至少 32 字符", 400)
		return
	}
	for i := range in.Cluster.Nodes {
		n := &in.Cluster.Nodes[i]
		if n.ID == "" {
			ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
			var identity config.ClusterNode
			err := cluster.Call(ctx, *n, c.Cluster.LocalID, "GET", "/cluster/identity", nil, &identity)
			cancel()
			if err != nil || identity.ID == "" {
				http.Error(w, "无法识别节点，请检查访问地址、令牌及目标版本", 400)
				return
			}
			n.ID, n.Name = identity.ID, identity.Name
		}
		if in.Cluster.Nodes[i].Token == "" {
			if n, ok := c.Node(in.Cluster.Nodes[i].ID); ok {
				in.Cluster.Nodes[i].Token = n.Token
			}
		}
		if n.ID == in.Cluster.LocalID {
			n.Token = in.Cluster.Token
			n.Name = strings.TrimSpace(n.Name)
			if n.Name == "" {
				n.Name = c.DefaultNodeName()
			}
		}
	}
	in.Cluster.Version = c.Cluster.Version
	for _, n := range in.Cluster.Nodes {
		if n.ID == in.Cluster.LocalID {
			continue
		}
		_, exists := c.Node(n.ID)
		if !exists || slices.Contains(c.Cluster.JoinTargets, n.ID) {
			in.Cluster.JoinTargets = append(in.Cluster.JoinTargets, n.ID)
		}
	}
	c.Cluster = in.Cluster
	for _, v := range in.Policies {
		found := false
		for i := range c.DnsConf {
			if c.DnsConf[i].ID == v.ID {
				c.DnsConf[i].Cluster = v.Cluster
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "配置已改变，请刷新页面", 409)
			return
		}
	}
	c.Audit = in.Audit
	c.ClusterDefaults()
	if len(cluster.Members(c)) > 1 {
		self, _ := c.Node(c.Cluster.LocalID)
		if self.URL == "" {
			http.Error(w, "请在关联节点中填写本机节点的访问地址，以便其他节点自动加入并回传配置", 400)
			return
		}
	}
	if err := c.ValidateCluster(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	bumpClusterVersion(&c)
	if err := c.SaveConfig(); err != nil {
		http.Error(w, "保存失败", 500)
		return
	}
	queueClusterSync()
	auditConfigChanges(r, old, c)
	for _, n := range old.Cluster.Nodes {
		next, ok := c.Node(n.ID)
		if !ok {
			recordChange(r, c, "node.delete", "", n.Name, redactNode(n), nil)
		} else if !reflect.DeepEqual(n, next) {
			recordChange(r, c, "node.edit", "", next.Name, redactNode(n), redactNode(next))
		}
	}
	for _, n := range c.Cluster.Nodes {
		found := false
		for _, prev := range old.Cluster.Nodes {
			if prev.ID == n.ID {
				found = true
			}
		}
		if !found {
			recordChange(r, c, "node.add", "", n.Name, nil, redactNode(n))
		}
	}
	if old.Cluster.LocalID != c.Cluster.LocalID || old.Cluster.Token != c.Cluster.Token {
		recordChange(r, c, "cluster.settings", "", c.Cluster.LocalID, map[string]string{"LocalID": old.Cluster.LocalID, "Token": "[redacted]"}, map[string]string{"LocalID": c.Cluster.LocalID, "Token": "[redacted]"})
	}
	if old.Audit != c.Audit {
		recordChange(r, c, "audit.settings", "", "", old.Audit.Driver, c.Audit.Driver)
	}
	jsonResponse(w, map[string]any{"status": "ok", "Cluster": c.Cluster})
}
func redactNode(n config.ClusterNode) config.ClusterNode { n.Token = "[redacted]"; return n }
func recordChange(r *http.Request, c config.Config, op, name, node string, before, after any) {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	audit.Record(audit.Event{Actor: c.Username, Host: util.GetRequestIPStr(r), Operation: op, Config: name, Node: node, Before: string(b), After: string(a), Result: "ok"})
}
func redactDNS(d config.DnsConfig) config.DnsConfig {
	d.Networks = append([]config.NodeNetwork(nil), d.Networks...)
	for i := range d.Networks {
		d.Networks[i].Ipv4.Cmd = "[redacted]"
		d.Networks[i].Ipv4.URL = "[redacted]"
		d.Networks[i].Ipv6.Cmd = "[redacted]"
		d.Networks[i].Ipv6.URL = "[redacted]"
	}
	d.DNS.ID = "[redacted]"
	d.DNS.Secret = "[redacted]"
	d.DNS.ExtParam = "[redacted]"
	d.Ipv4.Cmd = "[redacted]"
	d.Ipv6.Cmd = "[redacted]"
	d.Ipv4.URL = "[redacted]"
	d.Ipv6.URL = "[redacted]"
	return d
}
func auditConfigChanges(r *http.Request, old, c config.Config) {
	previous := map[string]config.DnsConfig{}
	for _, d := range old.DnsConf {
		previous[d.ID] = d
	}
	for _, d := range c.DnsConf {
		before, ok := previous[d.ID]
		if !ok {
			recordChange(r, old, "config.add", d.DisplayName(), c.Cluster.LocalID, nil, redactDNS(d))
		} else if !reflect.DeepEqual(before, d) {
			recordChange(r, old, "config.edit", d.DisplayName(), c.Cluster.LocalID, redactDNS(before), redactDNS(d))
		}
		delete(previous, d.ID)
	}
	for _, d := range previous {
		recordChange(r, old, "config.delete", d.DisplayName(), c.Cluster.LocalID, redactDNS(d), nil)
	}
	if old.User != c.User || old.NotAllowWanAccess != c.NotAllowWanAccess || old.Webhook != c.Webhook {
		recordChange(r, old, "settings.edit", "", c.Cluster.LocalID, map[string]any{"Username": old.Username, "NotAllowWanAccess": old.NotAllowWanAccess, "Credentials": "[redacted]"}, map[string]any{"Username": c.Username, "NotAllowWanAccess": c.NotAllowWanAccess, "Credentials": "[redacted]"})
	}
}
func AuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if audit.Default == nil {
		http.Error(w, "审计数据库未启动，请检查日志", 503)
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, err := audit.Default.Query(r.Context(), audit.Filter{Config: q.Get("config"), Node: q.Get("node"), Operation: q.Get("operation"), Offset: offset, Limit: limit})
	if err != nil {
		http.Error(w, "审计查询失败", 500)
		return
	}
	jsonResponse(w, struct {
		Events []audit.Event
		Lost   uint64
	}{events, audit.Default.Lost.Load()})
}
func ManagementPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	t, err := template.ParseFS(writingEmbedFile, "management.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	page := "cluster"
	if r.URL.Path == "/audit" {
		page = "audit"
	}
	if err := t.Execute(w, page); err != nil {
		return
	}
}
