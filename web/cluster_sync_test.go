package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
func TestLocalSessionAndLogout(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = nil
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	sessionMu.Lock()
	sessions = map[string]http.Cookie{"test": {Name: cookieName, Value: "test", Expires: time.Now().Add(time.Hour)}}
	sessionMu.Unlock()
	defer invalidateSessions()
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

}
func TestSeparatePageSavesPolicy(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = append(c.Cluster.Nodes, config.ClusterNode{ID: "self", URL: "http://self"})
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

func TestMultipleSessionsWithoutPeerLease(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = []config.ClusterNode{{ID: "offline", URL: "http://127.0.0.1:1", Token: strings.Repeat("x", 32)}}
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

func TestFirstJoinRegistersSenderAndSynchronizesConfig(t *testing.T) {
	c := setupCluster(t)
	c.Cluster.Nodes = nil
	c.Cluster.Version = config.SyncVersion{Counter: 100, Node: "self"}
	if err := c.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	s := sharedSnapshot(c)
	s.Version = config.SyncVersion{Counter: 1, Node: "new", Actor: "admin"}
	s.Nodes = []config.ClusterNode{{ID: "self", URL: "http://self", Token: c.Cluster.Token}, {ID: "new", URL: "http://new", Token: strings.Repeat("n", 32)}}
	s.Configs = []config.DnsConfig{{ID: "from-new", Name: "new configuration"}}
	body, _ := json.Marshal(s)
	for _, token := range []string{"wrong", c.Cluster.Token} {
		r := httptest.NewRequest("POST", "/cluster/join", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-DDNS-Node", "new")
		w := httptest.NewRecorder()
		PeerTokenAuth(ClusterJoin)(w, r)
		if token == "wrong" {
			if w.Code != 401 {
				t.Fatal("join accepted bad token")
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	got, _ := config.GetConfigCached()
	if n, ok := got.Node("new"); !ok || n.URL != "http://new" || n.Token != strings.Repeat("n", 32) {
		t.Fatal("sender not enrolled")
	}
	if got.Cluster.LocalID != "self" || got.Cluster.Token != c.Cluster.Token || got.Cluster.Version.Counter != 101 || got.DnsConf[0].ID != "from-new" {
		t.Fatal("join did not preserve identity and adopt source")
	}
}
