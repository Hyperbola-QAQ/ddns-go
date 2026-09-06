package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
)

func TestDecentralizedClaims(t *testing.T) {
	managers := []*Claims{NewClaims(), NewClaims(), NewClaims()}
	var nodes []config.ClusterNode
	for i, id := range []string{"a", "b", "c"} {
		m := managers[i]
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var q Claim
			json.NewDecoder(r.Body).Decode(&q)
			json.NewEncoder(w).Encode(m.Apply(q))
		}))
		t.Cleanup(server.Close)
		nodes = append(nodes, config.ClusterNode{ID: id, URL: server.URL})
	}
	cfg := func(id string) config.Config { return config.Config{Cluster: config.ClusterSettings{LocalID: id}} }
	// The first trigger executes on its own node; no permanent coordinator.
	first, err := acquire(context.Background(), cfg("b"), nodes, "dns/g", managers[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(context.Background(), cfg("a"), nodes, "dns/g", managers[0]); err == nil {
		t.Fatal("second executor admitted")
	}
	first.Release()
	for round := 0; round < 8; round++ {
		var wg sync.WaitGroup
		results := make([]*Lease, 3)
		start := make(chan struct{})
		for i := range nodes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], _ = acquire(context.Background(), cfg(nodes[i].ID), nodes, "dns/g", managers[i])
			}(i)
		}
		close(start)
		wg.Wait()
		count := 0
		for _, l := range results {
			if l != nil {
				count++
				if !l.Valid() {
					t.Fatal("invalid winning grant")
				}
				l.Release()
			}
		}
		if count != 1 {
			t.Fatalf("simultaneous triggers: %d winners", count)
		}
	}
	// A failed/partitioned peer blocks a new management login.
	missing := append(append([]config.ClusterNode(nil), nodes...), config.ClusterNode{ID: "offline", URL: "http://127.0.0.1:1"})
	if _, err := acquire(context.Background(), cfg("a"), missing, "session", managers[0]); err == nil {
		t.Fatal("login accepted without all peers")
	}
}
func TestClaimExpiryAndOwner(t *testing.T) {
	m := NewClaims()
	q := Claim{Scope: "session", Owner: "a", Ticket: "one", Action: "prepare"}
	m.Apply(q)
	q.Action = "commit"
	m.Apply(q)
	intruder := Claim{Scope: "session", Owner: "b", Ticket: "one", Action: "release"}
	if m.Apply(intruder).Accepted {
		t.Fatal("different owner released claim")
	}
	if !m.Holds("session", "a", "one") {
		t.Fatal("lost live claim")
	}
	m.mu.Lock()
	g := m.grants["session"]
	g.Until = time.Now().Add(-time.Second)
	m.grants["session"] = g
	m.mu.Unlock()
	if m.Holds("session", "a", "one") {
		t.Fatal("expired session still valid")
	}
	intruder.Action = "prepare"
	if !m.Apply(intruder).Accepted {
		t.Fatal("expired claim prevents takeover")
	}
}
