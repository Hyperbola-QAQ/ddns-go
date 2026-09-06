package cluster

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
)

var ErrBusy = errors.New("其他节点正在执行或管理此资源")

const ElectionWindow = 200 * time.Millisecond

type Claim struct {
	Scope   string
	Owner   string
	Ticket  string
	Action  string
	Seconds int
}
type Vote struct {
	Accepted bool
	Owner    string
}
type grant struct {
	Claim
	Held  bool
	Until time.Time
}

// Claims arbitrates short-lived peer proposals, without a fixed leader or wall-clock comparisons.
type Claims struct {
	mu     sync.Mutex
	grants map[string]grant
}

func NewClaims() *Claims { return &Claims{grants: map[string]grant{}} }

var LocalClaims = NewClaims()

func (m *Claims) Apply(q Claim) Vote {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	g := m.grants[q.Scope]
	if !now.Before(g.Until) {
		g = grant{}
	}
	same := g.Owner == q.Owner && g.Ticket == q.Ticket
	ttl := time.Duration(q.Seconds) * time.Second
	if q.Scope == "session" {
		ttl = 45 * time.Second
	} else {
		ttl = 120 * time.Second
	}
	switch q.Action {
	case "prepare":
		if !g.Held && (g.Ticket == "" || q.Ticket < g.Ticket || same) {
			g = grant{Claim: q, Until: now.Add(ttl)}
			same = true
		}
	case "commit":
		if same {
			g.Held = true
			g.Until = now.Add(ttl)
		}
	case "renew":
		if same && g.Held {
			g.Until = now.Add(ttl)
		} else {
			same = false
		}
	case "release":
		if same {
			delete(m.grants, q.Scope)
			return Vote{Accepted: true}
		}
		return Vote{}
	default:
		return Vote{}
	}
	m.grants[q.Scope] = g
	return Vote{Accepted: same, Owner: g.Owner}
}
func (m *Claims) Holds(scope, owner, ticket string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[scope]
	return g.Held && g.Owner == owner && g.Ticket == ticket && time.Now().Before(g.Until)
}

// Lease is immutable; the local grant and peer grants must all agree before execution.
type Lease struct {
	Claim   Claim
	Nodes   []config.ClusterNode
	LocalID string
	local   *Claims
}

func (l *Lease) apply(ctx context.Context, action string) bool {
	q := l.Claim
	q.Action = action
	var wg sync.WaitGroup
	results := make([]bool, len(l.Nodes))
	for i, n := range l.Nodes {
		wg.Add(1)
		go func(i int, n config.ClusterNode) {
			defer wg.Done()
			if n.ID == l.LocalID {
				results[i] = l.manager().Apply(q).Accepted
				return
			}
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var v Vote
			if request(call, n, l.LocalID, "POST", "/cluster/claim", q, &v) == nil {
				results[i] = v.Accepted
			}
		}(i, n)
	}
	wg.Wait()
	for _, ok := range results {
		if !ok {
			return false
		}
	}
	return len(results) > 0
}
func Acquire(ctx context.Context, c config.Config, nodes []config.ClusterNode, scope string) (*Lease, error) {
	return acquire(ctx, c, nodes, scope, LocalClaims)
}
func acquire(ctx context.Context, c config.Config, nodes []config.ClusterNode, scope string, manager *Claims) (*Lease, error) {
	l := &Lease{local: manager, Claim: Claim{Scope: scope, Owner: c.Cluster.LocalID, Ticket: rand.Text()}, Nodes: nodes, LocalID: c.Cluster.LocalID}
	if !l.apply(ctx, "prepare") {
		l.Release()
		return nil, ErrBusy
	}
	if len(nodes) > 1 {
		timer := time.NewTimer(ElectionWindow)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			l.Release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if !l.apply(ctx, "commit") {
		l.Release()
		return nil, ErrBusy
	}
	return l, nil
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	l.apply(ctx, "release")
}
func (l *Lease) Renew(ctx context.Context) bool { return l != nil && l.apply(ctx, "renew") }
func (l *Lease) Valid() bool {
	return l != nil && l.manager().Holds(l.Claim.Scope, l.LocalID, l.Claim.Ticket)
}
func Members(c config.Config) []config.ClusterNode {
	nodes := append([]config.ClusterNode(nil), c.Cluster.Nodes...)
	for _, n := range nodes {
		if n.ID == c.Cluster.LocalID {
			return nodes
		}
	}
	n, _ := c.Node(c.Cluster.LocalID)
	return append(nodes, n)
}
func IsMember(p config.ClusterPolicy, id string) bool {
	for _, member := range p.IDs() {
		if member == id {
			return true
		}
	}
	return false
}

// Call reuses the authenticated bounded peer transport for sync and session protocols.
func Call(ctx context.Context, n config.ClusterNode, local, method, path string, in, out any) error {
	return request(ctx, n, local, method, path, in, out)
}

// Reconcile extends a live session to the current topology without changing its ticket.
func (l *Lease) Reconcile(ctx context.Context, c config.Config) (*Lease, bool) {
	if !l.Valid() || l.LocalID != c.Cluster.LocalID {
		return nil, false
	}
	next := &Lease{Claim: l.Claim, Nodes: Members(c), LocalID: l.LocalID}
	if !next.apply(ctx, "prepare") || !next.apply(ctx, "commit") {
		next.Release()
		return nil, false
	}
	for _, old := range l.Nodes {
		found := false
		for _, n := range next.Nodes {
			if old.ID == n.ID {
				found = true
			}
		}
		if !found {
			removed := &Lease{Claim: l.Claim, Nodes: []config.ClusterNode{old}, LocalID: l.LocalID}
			removed.Release()
		}
	}
	return next, true
}

func (l *Lease) manager() *Claims {
	if l.local != nil {
		return l.local
	}
	return LocalClaims
}
