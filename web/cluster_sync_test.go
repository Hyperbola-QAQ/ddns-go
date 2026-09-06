package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeessy2/ddns-go/v6/cluster"
	"github.com/jeessy2/ddns-go/v6/config"
)

func TestSnapshotSynchronizesCompleteConfig(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = append(c.Cluster.Nodes, config.ClusterNode{ID: "self", URL: "http://self"})
	c.DnsConf = []config.DnsConfig{{ID: "old"}}
	sender := c
	sender.Cluster.LocalID = "peer"
	sender.Cluster.Token = strings.Repeat("b", 32)
	sender.Username = "operator"
	sender.Password = "shared-hash"
	d := config.DnsConfig{ID: "shared", Name: "after", DNS: config.DNS{Name: "callback", Secret: "new-secret"}, HttpInterface: "eth0", Networks: []config.NodeNetwork{{NodeID: "peer", Ipv6: config.IPConfig{Enable: true, GetType: "cmd", Cmd: "remote-command", Domains: []string{"peer.example.com"}}}}}
	sender.DnsConf = []config.DnsConfig{d}
	sender.ClusterDefaults()
	bumpClusterVersion(&sender)
	snapshot := sharedSnapshot(sender)
	next, err := mergeSnapshot(c, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.DnsConf) != 1 || next.DnsConf[0].Networks[0].Ipv6.Cmd != "remote-command" || next.DnsConf[0].DNS.Secret != "new-secret" || next.Password != "shared-hash" || next.Username != "operator" {
		t.Fatal("incomplete synchronization")
	}
	if next.Cluster.LocalID != c.Cluster.LocalID || next.Cluster.Token != c.Cluster.Token {
		t.Fatal("lost process identity")
	}
	snapshot.Configs = nil
	snapshot.Version.Counter++
	removed, err := mergeSnapshot(next, snapshot)
	if err != nil || len(removed.DnsConf) != 0 {
		t.Fatal("deletion not replicated", err)
	}
	stale, err := mergeSnapshot(removed, sharedSnapshot(sender))
	if err != nil || len(stale.DnsConf) != 0 {
		t.Fatal("stale snapshot resurrected config")
	}
}
func TestConfigPushAndRecovery(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = append(c.Cluster.Nodes, config.ClusterNode{ID: "self", URL: "http://self"})
	var remote SharedSnapshot
	offline := true
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline {
			http.Error(w, "offline", 503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 32) {
			t.Error("missing authentication")
		}
		if r.Method == "GET" {
			jsonResponse(w, remote)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&remote); err != nil {
			t.Error(err)
		}
		jsonResponse(w, map[string]string{"status": "ok"})
	}))
	defer peer.Close()
	c.Cluster.Nodes[0].URL = peer.URL
	bumpClusterVersion(&c)
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	syncClusterOnce()
	if len(clusterSyncErrors()) != 1 {
		t.Fatal("failed synchronization not visible")
	}
	offline = false
	syncClusterOnce()
	if remote.Version != c.Cluster.Version || len(clusterSyncErrors()) != 0 {
		t.Fatal("reconnected peer not repaired")
	}
}
func TestSingleLoginLeaseAndLogout(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = nil
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireLogin(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	sessionMu.Lock()
	loginLease = lease
	sessionSeen = map[string]time.Time{"test": time.Now()}
	sessions = map[string]http.Cookie{"test": {Name: cookieName, Value: "test", Expires: time.Now().Add(time.Hour)}}
	sessionMu.Unlock()
	defer func() { sessionMu.Lock(); loginLease = nil; sessions = map[string]http.Cookie{}; sessionMu.Unlock() }()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "test"})
	w := httptest.NewRecorder()
	Auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(w, req)
	if w.Code != 204 {
		t.Fatal("valid session rejected")
	}
	Logout(httptest.NewRecorder(), req)
	w = httptest.NewRecorder()
	Auth(func(w http.ResponseWriter, r *http.Request) { t.Error("logged out cookie accepted") })(w, req)
	if w.Code != 307 {
		t.Fatal("logout did not invalidate cookie")
	}
	if _, err := cluster.Acquire(context.Background(), c, cluster.Members(c), "session"); err != nil {
		t.Fatal("logout did not release cluster claim")
	}
	cluster.LocalClaims = cluster.NewClaims()
}
func TestSeparatePageSavesPolicy(t *testing.T) {
	c := setupCluster(t)
	c.DnsConf = []config.DnsConfig{{ID: "one", Name: "home"}}
	c.ClusterDefaults()
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	policies := policyViews(c)
	policies[0].Cluster.Enabled = true
	policies[0].Cluster.Group = "g"
	policies[0].Cluster.NodeIDs = "self"
	body, _ := json.Marshal(map[string]any{"Cluster": c.Cluster, "Audit": c.Audit, "Policies": policies})
	r := httptest.NewRequest("POST", "/cluster/settings", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ClusterSettings(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, _ := config.GetConfigCached()
	if !got.DnsConf[0].Cluster.Enabled || got.DnsConf[0].Cluster.Group != "g" {
		t.Fatal("policy not persisted")
	}
}

func TestMultipleSessionsStayOnOneNode(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = nil
	c.Password = ""
	c.Username = ""
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	defer invalidateSessions()
	login := func() *http.Cookie {
		body, _ := json.Marshal(map[string]string{"Username": "admin", "Password": "Strong-Test-Password-9876!"})
		w := httptest.NewRecorder()
		LoginFunc(w, httptest.NewRequest("POST", "/loginFunc", bytes.NewReader(body)))
		var result Result
		json.Unmarshal(w.Body.Bytes(), &result)
		if result.Code != 200 {
			t.Fatal(result.Msg)
		}
		return w.Result().Cookies()[0]
	}
	first := login()
	second := login()
	if first.Value == second.Value {
		t.Fatal("sessions share a token")
	}
	for _, cookie := range []*http.Cookie{first, second} {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		Auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(w, r)
		if w.Code != 204 {
			t.Fatal("same-node session rejected")
		}
	}
	req := httptest.NewRequest("GET", "/logout", nil)
	req.AddCookie(first)
	Logout(httptest.NewRecorder(), req)
	sessionMu.RLock()
	live := loginLease.Valid()
	sessionMu.RUnlock()
	if !live {
		t.Fatal("logging out one session released node occupancy")
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(second)
	w := httptest.NewRecorder()
	Auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(w, req)
	if w.Code != 204 {
		t.Fatal("other local session was invalidated")
	}
	Logout(httptest.NewRecorder(), req)
	sessionMu.RLock()
	remaining := len(sessions)
	sessionMu.RUnlock()
	if remaining != 0 {
		t.Fatal("last logout left sessions")
	}
}
