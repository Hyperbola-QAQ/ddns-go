package cluster

import (
	"errors"
	"math/rand/v2"
	"sync"

	"github.com/jeessy2/ddns-go/v6/config"
)

// Selector implementations must be concurrency-safe. Group is a stable config key.
type Selector interface {
	Select(group string, nodes []config.ClusterNode) (config.ClusterNode, error)
}
type randomSelector struct{}

func (randomSelector) Select(_ string, n []config.ClusterNode) (config.ClusterNode, error) {
	if len(n) == 0 {
		return config.ClusterNode{}, errors.New("没有存活节点")
	}
	return n[rand.IntN(len(n))], nil
}

type roundRobin struct {
	mu   sync.Mutex
	next map[string]uint64
}

func (r *roundRobin) Select(g string, n []config.ClusterNode) (config.ClusterNode, error) {
	if len(n) == 0 {
		return config.ClusterNode{}, errors.New("没有存活节点")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.next[g]
	r.next[g]++
	return n[i%uint64(len(n))], nil
}

var selectorMu sync.RWMutex
var selectors = map[string]Selector{"random": randomSelector{}, "round-robin": &roundRobin{next: map[string]uint64{}}}

func RegisterSelector(name string, s Selector) Selector {
	selectorMu.Lock()
	defer selectorMu.Unlock()
	old := selectors[name]
	selectors[name] = s
	config.RegisterClusterAlgorithm(name)
	return old
}
func Select(algorithm, group string, n []config.ClusterNode) (config.ClusterNode, error) {
	if algorithm == "" {
		algorithm = "random"
	}
	selectorMu.RLock()
	s := selectors[algorithm]
	selectorMu.RUnlock()
	if s == nil {
		return config.ClusterNode{}, errors.New("未知节点选择算法")
	}
	return s.Select(group, n)
}
