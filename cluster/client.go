package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
)

var Client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type Job struct {
	NodeID string
	IPv4   string
	IPv6   string
	Batch  bool
}
type UpdateRequest struct {
	LayoutHash string
	Group      string
	Jobs       []Job
}

func request(ctx context.Context, n config.ClusterNode, local, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.URL, "/")+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("X-DDNS-Node", local)
	req.Header.Set("Content-Type", "application/json")
	client := *Client
	if path == "/cluster/update" {
		client.Timeout = 90 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("节点 %s 返回 HTTP %d", n.ID, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Alive probes all members concurrently with a strict per-probe deadline and bounded fan-out.
func Alive(ctx context.Context, c config.Config, p config.ClusterPolicy) []config.ClusterNode {
	ids := p.IDs()
	result := make([]*config.ClusterNode, len(ids))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	for i, id := range ids {
		n, ok := c.Node(id)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(i int, n config.ClusterNode) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			if n.ID == c.Cluster.LocalID {
				result[i] = &n
				return
			}
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			// /ping deliberately carries no credentials.
			req, err := http.NewRequestWithContext(probe, http.MethodGet, strings.TrimRight(n.URL, "/")+"/ping", nil)
			if err != nil {
				return
			}
			resp, err := Client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var health struct{ Status string }
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 128)).Decode(&health) == nil && health.Status == "ok" {
				result[i] = &n
			}
		}(i, n)
	}
	wg.Wait()
	alive := []config.ClusterNode{}
	for _, n := range result {
		if n != nil {
			alive = append(alive, *n)
		}
	}
	return alive
}
func ReadIP(ctx context.Context, c config.Config, d config.DnsConfig, n config.ClusterNode) (config.ResolvedIP, error) {
	var ip config.ResolvedIP
	if err := ctx.Err(); err != nil {
		return ip, err
	}
	if n.ID == c.Cluster.LocalID {
		d.Resolved = nil
		if d.Ipv4.Enable {
			ip.IPv4 = d.GetIpv4Addr()
		}
		if d.Ipv6.Enable {
			ip.IPv6 = d.GetIpv6Addr()
		}
	} else {
		err := request(ctx, n, c.Cluster.LocalID, "GET", "/cluster/ip?group="+url.QueryEscape(d.Cluster.Group), nil, &ip)
		if err != nil {
			return ip, err
		}
	}
	if ip.IPv4 == "" && ip.IPv6 == "" {
		return ip, errors.New("节点未返回可用 IP")
	}
	return ip, ValidateIP(ip)
}
func Execute(ctx context.Context, c config.Config, n config.ClusterNode, r UpdateRequest) error {
	var result struct {
		Status string
		Error  string
	}
	if err := request(ctx, n, c.Cluster.LocalID, "POST", "/cluster/update", r, &result); err != nil {
		return err
	}
	if result.Status != "ok" {
		return errors.New("远程 DNS 更新失败: " + result.Error)
	}
	return nil
}
