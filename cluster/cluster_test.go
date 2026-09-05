package cluster

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jeessy2/ddns-go/v6/config"
)

func TestPrefixComposition(t *testing.T) {
	tests := []struct{ prefix, address, want string }{
		{"2001:db8:1234:5678::/64", "2001:db8:abcd:eeee:1234:5678:9abc:def0", "2001:db8:1234:5678:1234:5678:9abc:def0"},
		{"2001:db8:1234:5680::/57", "::7f:abcd:1234:5678:9abc", "2001:db8:1234:56ff:abcd:1234:5678:9abc"},
		{"2001:db8:1:2:ab80::/73", "::7f:1234:5678:9abc", "2001:db8:1:2:abff:1234:5678:9abc"},
		{"2001:db8::1/128", "::abcd", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			got, err := Compose(tt.prefix, tt.address)
			if err != nil || got != tt.want {
				t.Fatalf("got %s, %v; want %s", got, err, tt.want)
			}
		})
	}
	for _, ip := range []string{"192.0.2.1", "::ffff:192.0.2.1", "fe80::1%eth0", "bogus"} {
		if _, err := Prefix(ip, 64); err == nil {
			t.Fatalf("accepted %s", ip)
		}
	}
	if _, err := Prefix("2001:db8::1", 129); err == nil {
		t.Fatal("accepted /129")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAliveAndSelections(t *testing.T) {
	old := Client
	t.Cleanup(func() { Client = old })
	Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("ping exposed token")
		}
		status := 200
		body := `{"status":"ok"}`
		if r.URL.Host == "dead" {
			status = 503
		}
		if r.URL.Host == "invalid" {
			body = `{"status":"no"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	c := config.Config{Cluster: config.ClusterSettings{LocalID: "self", Nodes: []config.ClusterNode{{ID: "b", URL: "http://alive"}, {ID: "c", URL: "http://dead"}, {ID: "d", URL: "http://invalid"}}}}
	alive := Alive(context.Background(), c, config.ClusterPolicy{NodeIDs: "self,b,c,d"})
	if len(alive) != 2 || alive[0].ID != "self" || alive[1].ID != "b" {
		t.Fatalf("unexpected alive: %v", alive)
	}
	r := &roundRobin{next: map[string]uint64{}}
	for _, id := range []string{"self", "b", "self", "b"} {
		n, err := r.Select("g", alive)
		if err != nil || n.ID != id {
			t.Fatalf("got %v %v", n, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Select("g", alive) }()
	}
	wg.Wait()
	for i := 0; i < 100; i++ {
		n, err := Select("random", "g", alive)
		if err != nil || (n.ID != "self" && n.ID != "b") {
			t.Fatal("selected dead node")
		}
	}
	if _, err := Select("random", "g", nil); err == nil {
		t.Fatal("selected from empty list")
	}
}
func TestStateRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := map[string]*State{"g": {Prefix: "2001:db8::/64", PendingPrefix: "2001:db8:1::/64", Pending: []Job{{NodeID: "b", IPv6: "2001:db8:1::5", Batch: true}}}}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil || got["g"].Pending[0].IPv6 != "2001:db8:1::5" {
		t.Fatalf("%v %v", got, err)
	}
}
