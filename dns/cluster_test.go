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

func TestClusterPrefixLocalExecutionAndRetry(t *testing.T) {
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
	d := config.DnsConfig{Name: "home", DNS: config.DNS{Name: "callback", ID: provider.URL + "?domain=#{domain}&ip=#{ip}"}, Cluster: config.ClusterPolicy{Enabled: true, Group: "home", NodeIDs: "peer,offline", Source: "node", SourceNode: "peer", Algorithm: "random", PrefixLength: 64}}
	// The triggering executor may differ from the IP source.
	d.Cluster.NodeIDs = "self,peer,offline"
	d.Ipv6.Enable = true
	d.Ipv6.GetType = "cmd"
	d.Ipv6.Cmd = "printf '2001:db8:1::1'"
	d.Ipv6.Domains = []string{"service.example.com"}
	var probes, updates atomic.Int32
	peerClaims := cluster.NewClaims()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			probes.Add(1)
			w.Write([]byte(`{"status":"ok"}`))
		case "/cluster/ip":
			json.NewEncoder(w).Encode(config.ResolvedIP{IPv6: ip.Load().(string)})
		case "/cluster/claim":
			var q cluster.Claim
			json.NewDecoder(r.Body).Decode(&q)
			json.NewEncoder(w).Encode(peerClaims.Apply(q))
		case "/cluster/state":
			if r.Method == "GET" {
				json.NewEncoder(w).Encode(cluster.State{})
			} else {
				updates.Add(1)
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			}

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
	if updates.Load() < 1 || probes.Load() < 2 {
		t.Fatal("batch did not re-probe and broadcast completion")
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
	d := config.DnsConfig{Cluster: config.ClusterPolicy{Enabled: true, Group: "g", NodeIDs: "self"}}
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
	d := config.DnsConfig{Cluster: config.ClusterPolicy{Enabled: true, Group: "g", NodeIDs: "self"}}
	d.Ipv4.Enable = true
	d.Ipv4.Domains = []string{"", " "}
	r := cluster.UpdateRequest{Group: "g", LayoutHash: cluster.LayoutHash(c, d), Jobs: []cluster.Job{{NodeID: "self", IPv6: "2001:db8::1"}}}
	if err := ExecuteCluster(c, d, r, "self"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeNetworksUpdateOwnDomainsAndCustomSuffixes(t *testing.T) {
	t.Setenv(util.ConfigFilePathENV, filepath.Join(t.TempDir(), "config.yaml"))
	var mu sync.Mutex
	records := map[string]string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		records[r.URL.Query().Get("domain")] = r.URL.Query().Get("ip")
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	defer provider.Close()
	var ip atomic.Value
	ip.Store("2001:db8:1::2")
	claims := cluster.NewClaims()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			w.Write([]byte(`{"status":"ok"}`))
		case "/cluster/ip":
			json.NewEncoder(w).Encode(config.ResolvedIP{IPv4: "192.0.2.2", IPv6: ip.Load().(string)})
		case "/cluster/claim":
			var q cluster.Claim
			json.NewDecoder(r.Body).Decode(&q)
			json.NewEncoder(w).Encode(claims.Apply(q))
		case "/cluster/state":
			json.NewEncoder(w).Encode(cluster.State{})
		}
	}))
	defer peer.Close()
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self", Token: strings.Repeat("a", 32), Nodes: []config.ClusterNode{{ID: "peer", URL: peer.URL, Token: strings.Repeat("b", 32)}}}}
	d := config.DnsConfig{ID: "networks", DNS: config.DNS{Name: "callback", ID: provider.URL + "?domain=#{domain}&ip=#{ip}"}, Cluster: config.ClusterPolicy{Enabled: true, Group: "g", NodeIDs: "self,peer"}, Networks: []config.NodeNetwork{
		{NodeID: "self", Ipv4: config.IPConfig{Enable: true, GetType: "cmd", Cmd: "printf '192.0.2.1'", Domains: []string{"self4.example.com"}}, Ipv6: config.IPConfig{Enable: true, GetType: "cmd", Cmd: "printf '2001:db8:1::1'", Domains: []string{"self6.example.com"}}},
		{NodeID: "peer", Ipv4: config.IPConfig{Enable: true, GetType: "cmd", Cmd: "printf '192.0.2.2'", Domains: []string{"peer4.example.com"}}, Ipv6: config.IPConfig{Enable: true, GetType: "cmd", Cmd: "printf '2001:db8:1::2'", Domains: []string{"peer6.example.com", "custom.example.com"}}, Suffixes: "::2\n::1000"},
	}}
	c.DnsConf = []config.DnsConfig{d}
	c.ClusterDefaults()
	d = c.DnsConf[0]
	if err := runCluster(c, d); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if records["self4.example.com"] != "192.0.2.1" || records["peer4.example.com"] != "192.0.2.2" || records["custom.example.com"] != "2001:db8:1::1000" {
		t.Fatal(records)
	}
	mu.Unlock()
	ip.Store("2001:db8:2::2")
	if err := runCluster(c, d); err != nil {
		t.Fatal(err)
	}
	// The peer briefly reports the retired prefix; regular jobs must not write it back.
	ip.Store("2001:db8:1::2")
	if err := runCluster(c, d); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if records["self6.example.com"] != "2001:db8:2::1" || records["peer6.example.com"] != "2001:db8:2::2" || records["custom.example.com"] != "2001:db8:2::1000" {
		t.Fatal("stale prefix escaped debounce", records)
	}
}

func TestBlankSuffixKeepsFirstDomainHostBits(t *testing.T) {
	n := config.NodeNetwork{NodeID: "a", Ipv6: config.IPConfig{Enable: true, Domains: []string{"a.example.com", "b.example.com"}}, Suffixes: "\n::1000"}
	jobs, err := networkJobs(n, config.ResolvedIP{IPv6: "2001:db8:1::12"}, "2001:db8:2::/64", true)
	if err != nil || len(jobs) != 2 || jobs[0].IPv6 != "2001:db8:2::12" || jobs[1].IPv6 != "2001:db8:2::1000" {
		t.Fatal("suffix line alignment lost", jobs, err)
	}
}

func TestOneGlobalIPv6SuffixList(t *testing.T) {
	d := config.DnsConfig{IPv6Suffix: &config.SuffixConfig{Enable: true, IPs: "::1000\n\n::2000"}, Networks: []config.NodeNetwork{
		{NodeID: "a", Ipv6: config.IPConfig{Enable: true, Domains: []string{"a.example.com", "a2.example.com"}}, Suffixes: "::dead"},
		{NodeID: "b", Ipv6: config.IPConfig{Enable: true, Domains: []string{"b.example.com", "b2.example.com"}}},
	}}
	for _, tt := range []struct {
		id   string
		want []string
	}{{"a", []string{"2001:db8::1000", "2001:db8::12"}}, {"b", []string{"2001:db8::2000", "2001:db8::12"}}} {
		n, _ := d.NetworkWithSuffix(tt.id)
		jobs, err := networkJobs(n, config.ResolvedIP{IPv6: "2001:db8:1::12"}, "2001:db8::/64", true)
		if err != nil || len(jobs) != 2 {
			t.Fatal(err)
		}
		for i := range jobs {
			if jobs[i].IPv6 != tt.want[i] {
				t.Fatal(jobs)
			}
		}
	}
	d.IPv6Suffix.Enable = false
	n, _ := d.NetworkWithSuffix("a")
	if n.Suffixes != "" {
		t.Fatal("disabled global setting used legacy per-node suffix")
	}
}
