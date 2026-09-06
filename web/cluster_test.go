package web

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jeessy2/ddns-go/v6/audit"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

func setupCluster(t *testing.T) config.Config {
	t.Helper()
	t.Setenv(util.ConfigFilePathENV, filepath.Join(t.TempDir(), "config.yaml"))
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self", Token: strings.Repeat("a", 32), Nodes: []config.ClusterNode{{ID: "peer", URL: "http://peer", Token: strings.Repeat("b", 32)}}}}
	c.Username = "admin"
	c.Password = "hashed-placeholder"
	c.ClusterDefaults()
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestPingAndPeerAuth(t *testing.T) {
	c := setupCluster(t)
	w := httptest.NewRecorder()
	Ping(w, httptest.NewRequest("GET", "/ping", nil))
	if w.Code != 200 || w.Body.String() != `{"status":"ok"}` {
		t.Fatal(w.Body.String())
	}
	protected := PeerAuth(func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, "ok") })
	for _, tt := range []struct {
		token, node string
		code        int
	}{{"", "peer", 401}, {c.Cluster.Token, "unknown", 403}, {c.Cluster.Token, "peer", 200}} {
		r := httptest.NewRequest("GET", "/cluster/ip", nil)
		r.Header.Set("Authorization", "Bearer "+tt.token)
		r.Header.Set("X-DDNS-Node", tt.node)
		w := httptest.NewRecorder()
		protected(w, r)
		if w.Code != tt.code {
			t.Fatalf("got %d want %d", w.Code, tt.code)
		}
	}
}
func TestClusterSettingsShowTokensAndRejectCSRF(t *testing.T) {
	c := setupCluster(t)
	w := httptest.NewRecorder()
	ClusterSettings(w, httptest.NewRequest("GET", "/cluster/settings", nil))
	if !strings.Contains(w.Body.String(), c.Cluster.Token) || !strings.Contains(w.Body.String(), c.Cluster.Nodes[0].Token) {
		t.Fatal("authenticated settings page cannot display tokens")
	}
	c.Cluster.Token = ""
	c.Cluster.Nodes[0].Token = ""
	c.Cluster.Nodes[0].Name = "edited"
	body, _ := json.Marshal(struct {
		Cluster config.ClusterSettings
		Audit   config.AuditSettings
	}{c.Cluster, c.Audit})
	r := httptest.NewRequest("POST", "http://localhost/cluster/settings", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://evil.example")
	w = httptest.NewRecorder()
	ClusterSettings(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	r = httptest.NewRequest("POST", "/cluster/settings", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ClusterSettings(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	saved, _ := config.GetConfigCached()
	if saved.Cluster.Token != strings.Repeat("a", 32) || saved.Cluster.Nodes[0].Token != strings.Repeat("b", 32) || saved.Cluster.Nodes[0].Name != "edited" {
		t.Fatal("partial edit lost token")
	}
}
func TestManagementTemplates(t *testing.T) {
	setupCluster(t)
	for _, path := range []string{"/cluster", "/audit"} {
		w := httptest.NewRecorder()
		ManagementPage(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "DDNS-GO") {
			t.Fatal(w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	Writing(w, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(w.Body.String(), "ClusterPrefixLength") || strings.Contains(w.Body.String(), "ZgotmplZ") {
		t.Fatal("config template failed")
	}
}
func TestDNSConfigJSONRoundTrip(t *testing.T) {
	c := setupCluster(t)
	d := config.DnsConfig{Name: "one", Cluster: config.ClusterPolicy{Enabled: true, Group: "g", NodeIDs: "self", Source: "prefix", SourceNode: "self", Algorithm: "round-robin", PrefixLength: 57, CooldownSeconds: 60, Suffix: "::12"}}
	c.DnsConf = []config.DnsConfig{d}
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	var entries []dnsConf4JS
	if err := json.Unmarshal([]byte(getDnsConfStr(c.DnsConf)), &entries); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"Username": c.Username, "DnsConf": entries}
	b, _ := json.Marshal(data)
	r := httptest.NewRequest("POST", "/save", bytes.NewReader(b))
	if result := checkAndSave(r); result != "ok" {
		t.Fatal(result)
	}
	saved, _ := config.GetConfigCached()
	if saved.DnsConf[0].Cluster != d.Cluster {
		t.Fatalf("lost cluster fields: %+v", saved.DnsConf[0].Cluster)
	}
}

func TestDeletingEarlierConfigPreservesCredentialsAndAuditIdentity(t *testing.T) {
	c := setupCluster(t)
	for _, id := range []string{"first", "second"} {
		d := config.DnsConfig{ID: id, Name: id, DNS: config.DNS{Name: "alidns", ID: id + "-access-key", Secret: id + "-secret"}}
		d.Ipv4.Domains = []string{""}
		d.Ipv6.Domains = []string{""}
		c.DnsConf = append(c.DnsConf, d)
	}
	c.ClusterDefaults()
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	var entries []dnsConf4JS
	if err := json.Unmarshal([]byte(getDnsConfStr(c.DnsConf)), &entries); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"Username": c.Username, "DnsConf": entries[1:]})
	path := filepath.Join(t.TempDir(), "audit.db")
	writer, err := audit.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	audit.Default = writer
	result := checkAndSave(httptest.NewRequest("POST", "/save", bytes.NewReader(data)))
	audit.Default = nil
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Fatal(result)
	}
	saved, _ := config.GetConfigCached()
	if len(saved.DnsConf) != 1 || saved.DnsConf[0].DNS.Secret != "second-secret" {
		t.Fatal("deleting an earlier config lost remaining credentials")
	}
	reader, err := audit.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	events, err := reader.Query(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Operation != "config.delete" || events[0].Config != "first" {
		t.Fatalf("incorrect audit identity: %+v", events)
	}
	if strings.Contains(events[0].Before, "first-secret") {
		t.Fatal("audit leaked secret")
	}
}

func TestNodeInterfacesReadsTargetNode(t *testing.T) {
	c := setupCluster(t)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster/netinterfaces" || r.Header.Get("X-DDNS-Node") != "self" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 32) {
			t.Error("wrong peer request")
		}
		jsonResponse(w, map[string]any{"Ipv4": []config.NetInterface{{Name: "remote-eth", Address: []string{"192.0.2.2"}}}, "Ipv6": []config.NetInterface{{Name: "remote-eth", Address: []string{"2001:db8::2"}, Lifetimes: []config.AddressLifetime{{Known: true, ValidSeconds: 600}}}}})
	}))
	defer peer.Close()
	c.Cluster.Nodes[0].URL = peer.URL
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	NodeInterfaces(w, httptest.NewRequest("GET", "/cluster/interfaces?node=peer", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "remote-eth") || !strings.Contains(w.Body.String(), "600") {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	NodeInterfaces(w, httptest.NewRequest("GET", "/cluster/interfaces?node=unknown", nil))
	if w.Code != 404 {
		t.Fatal("unknown node accepted")
	}
}
