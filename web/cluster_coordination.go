package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

func ClusterClaim(w http.ResponseWriter, r *http.Request) {
	if !jsonMutation(w, r) {
		return
	}
	var q cluster.Claim
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil || q.Owner != r.Header.Get("X-DDNS-Node") || q.Ticket == "" || len(q.Ticket) > 128 {
		http.Error(w, "invalid claim", 400)
		return
	}
	c, _ := config.GetConfigCached()
	if q.Scope != "session" {
		d, ok := groupConfig(c, strings.TrimPrefix(q.Scope, "dns/"))
		if !strings.HasPrefix(q.Scope, "dns/") || !ok || !cluster.IsMember(d.Cluster, q.Owner) {
			http.Error(w, "group not available", 403)
			return
		}
	}
	jsonResponse(w, cluster.LocalClaims.Apply(q))
}
func ClusterState(w http.ResponseWriter, r *http.Request) {
	c, _ := config.GetConfigCached()
	group := r.URL.Query().Get("group")
	var in struct {
		Group  string
		Ticket string
		State  cluster.State
	}
	if r.Method != "GET" {
		if !jsonMutation(w, r) {
			return
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
			http.Error(w, "invalid state", 400)
			return
		}
		group = in.Group
	}
	d, ok := groupConfig(c, group)
	actor := r.Header.Get("X-DDNS-Node")
	if !ok || !cluster.IsMember(d.Cluster, actor) {
		http.Error(w, "group not available", 403)
		return
	}
	path := util.GetConfigFilePath() + ".cluster-state.json"
	states, err := cluster.LoadState(path)
	if err != nil {
		http.Error(w, "state unavailable", 500)
		return
	}
	if r.Method == "GET" {
		s := states[group]
		if s == nil {
			s = &cluster.State{}
		}
		jsonResponse(w, s)
		return
	}
	if !cluster.LocalClaims.Holds("dns/"+group, actor, in.Ticket) || in.State.LayoutHash != cluster.LayoutHash(c, d) {
		http.Error(w, "execution claim or layout mismatch", 409)
		return
	}
	if old := states[group]; old == nil || in.State.Version.Newer(old.Version) {
		if err := cluster.SaveGroup(path, group, &in.State); err != nil {
			http.Error(w, "state save failed", 500)
			return
		}
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// ClusterNetInterfaces returns this node's actual interfaces and address lifetimes.
func ClusterNetInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	v4, v6, err := config.GetNetInterface()
	if err != nil {
		http.Error(w, "读取网卡失败", 503)
		return
	}
	jsonResponse(w, struct{ Ipv4, Ipv6 []config.NetInterface }{v4, v6})
}
func NodeInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	c, _ := config.GetConfigCached()
	id := r.URL.Query().Get("node")
	n, ok := c.Node(id)
	if !ok {
		http.Error(w, "节点不存在", 404)
		return
	}
	if id == c.Cluster.LocalID {
		ClusterNetInterfaces(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	var out struct{ Ipv4, Ipv6 []config.NetInterface }
	if err := cluster.Call(ctx, n, c.Cluster.LocalID, "GET", "/cluster/netinterfaces", nil, &out); err != nil {
		http.Error(w, "节点暂不可达，保留已保存的网卡设置", 503)
		return
	}
	jsonResponse(w, out)
}
