package dns

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

type choosePeer struct{}

func (choosePeer) Select(_ string, nodes []config.ClusterNode) (config.ClusterNode, error) {
	for _, n := range nodes {
		if n.ID == "peer" {
			return n, nil
		}
	}
	return cluster.Select("round-robin", "fallback", nodes)
}

func TestClusterPrefixRemoteExecutionAndRetry(t *testing.T) {
	t.Setenv(util.ConfigFilePathENV, filepath.Join(t.TempDir(), "config.yaml"))
	var mu sync.Mutex
	records := map[string]string{}
	fail := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(w, "retry", 500)
			return
		}
		records[r.URL.Query().Get("domain")] = r.URL.Query().Get("ip")
		w.Write([]byte("ok"))
	}))
	defer provider.Close()
	var ip atomic.Value
	ip.Store("2001:db8:1::a")
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self", Token: strings.Repeat("s", 32)}}
	d := config.DnsConfig{Name: "home", DNS: config.DNS{Name: "callback", ID: provider.URL + "?domain=#{domain}&ip=#{ip}"}, Cluster: config.ClusterPolicy{Enabled: true, Group: "home", NodeIDs: "peer,offline", Coordinator: "peer", Source: "node", SourceNode: "peer", Algorithm: "random", PrefixLength: 64}}
	// Scheduler identity can be independent of the selected source/executor.
	d.Cluster.NodeIDs = "self,peer,offline"
	d.Cluster.Coordinator = "self"
	d.Ipv6.Enable = true
	d.Ipv6.GetType = "cmd"
	d.Ipv6.Cmd = "printf '2001:db8:1::1'"
	d.Ipv6.Domains = []string{"service.example.com"}
	var probes, updates atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			probes.Add(1)
			w.Write([]byte(`{"status":"ok"}`))
		case "/cluster/ip":
			json.NewEncoder(w).Encode(config.ResolvedIP{IPv6: ip.Load().(string)})
		case "/cluster/update":
			updates.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("p", 32) {
				t.Error("missing peer auth")
			}
			var req cluster.UpdateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			// Execute the real provider code as the remote worker, using its own local config.
			remote := c
			remote.Cluster.LocalID = "peer"
			err := ExecuteCluster(remote, d, req, "self")
			status := "ok"
			if err != nil {
				status = "failed"
			}
			json.NewEncoder(w).Encode(map[string]string{"Status": status})
		}
	}))
	defer peer.Close()
	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", 503) }))
	defer offline.Close()
	c.Cluster.Nodes = []config.ClusterNode{
		{ID: "self", Name: "router", Group: "home", Suffix: "::1", IPv6Domains: "router.example.com"},
		{ID: "peer", Name: "peer", URL: peer.URL, Group: "home", Token: strings.Repeat("p", 32), Suffix: "::a", IPv6Domains: "peer.example.com"},
		{ID: "offline", Name: "sleeping", URL: offline.URL, Group: "home", Token: strings.Repeat("x", 32), Suffix: "::ff", IPv6Domains: "sleeping.example.com"},
	}
	c.DnsConf = []config.DnsConfig{d}
	// Select peer deterministically without changing production algorithm configuration.
	oldSelector := cluster.RegisterSelector("random", choosePeer{})
	defer cluster.RegisterSelector("random", oldSelector)
	if err := runCluster(c, d); err != nil {
		t.Fatal(err)
	}
	if updates.Load() < 2 || probes.Load() < 2 {
		t.Fatal("batch did not re-probe and execute remotely")
	}
	mu.Lock()
	if records["sleeping.example.com"] != "2001:db8:1::ff" {
		t.Error("offline node suffix not propagated")
	}
	fail = true
	mu.Unlock()
	ip.Store("2001:db8:2::a")
	if err := runCluster(c, d); err == nil {
		t.Fatal("provider failure was ignored")
	}
	states, err := cluster.LoadState(util.GetConfigFilePath() + ".cluster-state.json")
	if err != nil {
		t.Fatal(err)
	}
	if states["home"].Prefix != "2001:db8:1::/64" || len(states["home"].Pending) != 3 {
		t.Fatalf("failed batch not persisted: %+v", states["home"])
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := runCluster(c, d); err != nil {
		t.Fatal(err)
	}
	states, err = cluster.LoadState(util.GetConfigFilePath() + ".cluster-state.json")
	if err != nil {
		t.Fatal(err)
	}
	if states["home"].Prefix != "2001:db8:2::/64" || len(states["home"].Pending) != 0 {
		t.Fatal("retry did not commit prefix")
	}
	mu.Lock()
	defer mu.Unlock()
	for domain, suffix := range map[string]string{"router.example.com": "1", "peer.example.com": "a", "sleeping.example.com": "ff"} {
		if records[domain] != "2001:db8:2::"+suffix {
			t.Errorf("%s: %s", domain, records[domain])
		}
	}
}

func TestExecuteClusterRejectsUntrustedPlan(t *testing.T) {
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self"}}
	d := config.DnsConfig{Cluster: config.ClusterPolicy{Enabled: true, Group: "g", Coordinator: "self", NodeIDs: "self"}}
	r := cluster.UpdateRequest{Group: "g", LayoutHash: cluster.LayoutHash(c, d), Jobs: []cluster.Job{{NodeID: "self", IPv6: "2001:db8::1"}}}
	if err := ExecuteCluster(c, d, r, "intruder"); err == nil {
		t.Fatal("non-coordinator accepted")
	}
	r.LayoutHash = "mismatch"
	if err := ExecuteCluster(c, d, r, "self"); err == nil {
		t.Fatal("divergent configuration accepted")
	}
	r.LayoutHash = cluster.LayoutHash(c, d)
	r.Jobs[0].IPv6 = "::ffff:192.0.2.1"
	if err := ExecuteCluster(c, d, r, "self"); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestIPv6OnlyIgnoresBlankIPv4Domain(t *testing.T) {
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self"}}
	d := config.DnsConfig{Cluster: config.ClusterPolicy{Enabled: true, Group: "g", Coordinator: "self", NodeIDs: "self"}}
	d.Ipv4.Enable = true
	d.Ipv4.Domains = []string{"", " "}
	r := cluster.UpdateRequest{Group: "g", LayoutHash: cluster.LayoutHash(c, d), Jobs: []cluster.Job{{NodeID: "self", IPv6: "2001:db8::1"}}}
	if err := ExecuteCluster(c, d, r, "self"); err != nil {
		t.Fatal(err)
	}
}
