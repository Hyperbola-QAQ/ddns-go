package dns

import (
	"encoding/json"
	"github.com/jeessy2/ddns-go/v6/audit"
	"github.com/jeessy2/ddns-go/v6/cluster"
	"strings"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

// DNS interface
type DNS interface {
	Init(dnsConf *config.DnsConfig, ipv4cache *util.IpCache, ipv6cache *util.IpCache)
	// 添加或更新IPv4/IPv6记录
	AddUpdateDomainRecords() (domains config.Domains)
}

var (
	Addresses = []string{
		alidnsEndpoint,
		aliesaEndpoint,
		baiduEndpoint,
		zonesAPI,
		recordListAPI,
		huaweicloudEndpoint,
		nameCheapEndpoint,
		nameSiloListRecordEndpoint,
		porkbunEndpoint,
		tencentCloudEndPoint,
		dynadotEndpoint,
		dynv6Endpoint,
		gcoreAPIEndpoint,
		edgeoneEndPoint,
		rainyunEndpoint,
		CloudnsEndpoint,
		desecEndpoint,
	}

	Ipcache = [][2]util.IpCache{}
)

// RunTimer 定时运行
var runRequests = make(chan struct{}, 1)

// RequestRun coalesces Web edits and avoids starting an unbounded queue of goroutines.
func RequestRun() {
	util.ForceCompareGlobal.Store(true)
	select {
	case runRequests <- struct{}{}:
	default:
	}
}

func RunTimer(delay time.Duration) {
	timer := time.NewTicker(delay)
	defer timer.Stop()
	for {
		RunOnce()
		select {
		case <-timer.C:
		case <-runRequests:
		}
	}
}

// RunOnce RunOnce
var runMu sync.Mutex
var updateMu sync.Mutex

func RunOnce() {
	runMu.Lock()
	defer runMu.Unlock()
	conf, err := config.GetConfigCached()
	if err != nil {
		return
	}
	if util.ForceCompareGlobal.Swap(false) || len(Ipcache) != len(conf.DnsConf) {
		Ipcache = [][2]util.IpCache{}
		for range conf.DnsConf {
			Ipcache = append(Ipcache, [2]util.IpCache{{}, {}})
		}
	}

	for i, dc := range conf.DnsConf {
		if !dc.Cluster.Enabled {
			for _, network := range dc.Networks {
				if (dc.IPv6Suffix != nil && dc.IPv6Suffix.Enable) || (dc.IPv6Suffix == nil && strings.TrimSpace(network.Suffixes) != "") {
					dc.Cluster.Enabled = true
					if dc.Cluster.Group == "" {
						dc.Cluster.Group = dc.ID
					}
					dc.Cluster.NodeIDs = conf.Cluster.LocalID
					conf.DnsConf[i] = dc
					break
				}
			}
		}

		if dc.Cluster.Enabled {
			if cluster.IsMember(dc.Cluster, conf.Cluster.LocalID) {
				if err := runCluster(conf, dc); err != nil {
					util.Log("集群配置 %s 更新失败: %s", dc.DisplayName(), err)
				}
			}
			continue
		}
		updateMu.Lock()
		previous, _ := json.Marshal(config.ResolvedIP{IPv4: Ipcache[i][0].Addr, IPv6: Ipcache[i][1].Addr})
		dc = dc.ForNode(conf.Cluster.LocalID)
		dnsSelected := newProvider(dc.DNS.Name)
		dnsSelected.Init(&dc, &Ipcache[i][0], &Ipcache[i][1])
		domains := dnsSelected.AddUpdateDomainRecords()
		// webhook
		v4Status, v6Status := config.ExecWebhook(&domains, &conf)
		if v4Status != config.UpdatedNothing || v6Status != config.UpdatedNothing {
			next, _ := json.Marshal(config.ResolvedIP{IPv4: domains.Ipv4Addr, IPv6: domains.Ipv6Addr})
			result := "ok"
			if v4Status == config.UpdatedFailed || v6Status == config.UpdatedFailed {
				result = "failed"
			}
			audit.Record(audit.Event{Actor: "scheduler", Host: conf.Cluster.LocalID, Operation: "dns.update", Config: dc.DisplayName(), Node: conf.Cluster.LocalID, Before: string(previous), After: string(next), Result: result})
		}
		// 重置单个cache
		if v4Status == config.UpdatedFailed {
			Ipcache[i][0] = util.IpCache{}
		}
		if v6Status == config.UpdatedFailed {
			Ipcache[i][1] = util.IpCache{}
		}
		updateMu.Unlock()
	}

}

func newProvider(name string) DNS {
	var dnsSelected DNS
	switch name {
	case "alidns":
		dnsSelected = &Alidns{}
	case "aliesa":
		dnsSelected = &Aliesa{}
	case "tencentcloud":
		dnsSelected = &TencentCloud{}
	case "trafficroute":
		dnsSelected = &TrafficRoute{}
	case "dnspod":
		dnsSelected = &Dnspod{}
	case "dnsla":
		dnsSelected = &Dnsla{}
	case "cloudflare":
		dnsSelected = &Cloudflare{}
	case "huaweicloud":
		dnsSelected = &Huaweicloud{}
	case "callback":
		dnsSelected = &Callback{}
	case "baiducloud":
		dnsSelected = &BaiduCloud{}
	case "porkbun":
		dnsSelected = &Porkbun{}
	case "godaddy":
		dnsSelected = &GoDaddyDNS{}
	case "namecheap":
		dnsSelected = &NameCheap{}
	case "namesilo":
		dnsSelected = &NameSilo{}
	case "vercel":
		dnsSelected = &Vercel{}
	case "dynadot":
		dnsSelected = &Dynadot{}
	case "dynv6":
		dnsSelected = &Dynv6{}
	case "spaceship":
		dnsSelected = &Spaceship{}
	case "nowcn":
		dnsSelected = &Nowcn{}
	case "eranet":
		dnsSelected = &Eranet{}
	case "tnethk":
		dnsSelected = &Tnethk{}
	case "gcore":
		dnsSelected = &Gcore{}
	case "edgeone":
		dnsSelected = &EdgeOne{}
	case "nsone":
		dnsSelected = &NSOne{}
	case "name_com":
		dnsSelected = &NameCom{}
	case "rainyun":
		dnsSelected = &Rainyun{}
	case "hipmdnsmgr":
		dnsSelected = &HiPMDnsMgr{}
	case "cloudns":
		dnsSelected = &ClouDNS{}
	case "desec":
		dnsSelected = &DeSEC{}
	default:
		dnsSelected = &Alidns{}
	}

	return dnsSelected
}
