# DDNS-GO

[![GitHub release](https://img.shields.io/github/release/jeessy2/ddns-go.svg?logo=github&style=flat-square) ![GitHub release downloads](https://img.shields.io/github/downloads/jeessy2/ddns-go/total?logo=github)](https://github.com/jeessy2/ddns-go/releases/latest) [![Go version](https://img.shields.io/github/go-mod/go-version/jeessy2/ddns-go)](https://github.com/jeessy2/ddns-go/blob/master/go.mod) [![](https://goreportcard.com/badge/github.com/jeessy2/ddns-go/v6)](https://goreportcard.com/report/github.com/jeessy2/ddns-go/v6) [![](https://img.shields.io/docker/image-size/jeessy/ddns-go)](https://registry.hub.docker.com/r/jeessy/ddns-go) [![](https://img.shields.io/docker/pulls/jeessy/ddns-go)](https://registry.hub.docker.com/r/jeessy/ddns-go)

# 分布式集群使用说明

## 工作方式

集群功能按配置启用；未启用的旧配置继续使用原有更新流程。每个配置组有一个明确的调度节点，它在更新前探活关联节点，选取 IP 来源及执行节点。执行节点调用自己的 DNS 服务商配置，不接收远程传入的密钥、命令或获取地址设置。

这是一种单调度节点、多个执行节点的轻量实现，不包含共识选举。执行节点掉线时，后续任务可选择其他存活节点；调度节点掉线时暂停该组更新，恢复它或在所有成员上统一修改调度节点 ID。网络分区时不要同时启用两个调度节点。

## 配置步骤

1. 在每台主机部署此版本，启用 Web 服务。`-noweb` 不适合集群节点，因为其他成员需要访问节点接口。
2. 登录“集群管理”，将本机 ID 设置为全组唯一的稳定值，如 `router-a`。生成并保存本机令牌，将它填写到其他主机上对应节点的访问令牌中。名称可独立编辑，ID 不应随意变更。
3. 添加组内所有节点，包括本机；填写名称、访问地址、配置组标识。远程 URL 支持 HTTP/HTTPS 和反向代理路径前缀。本机地址可留空，进程自身视为存活。公网通信请使用 HTTPS 或受保护的专用网络。
4. 在配置管理中启用集群，设置唯一组标识，如 `home`，关联节点 `router-a,server-b`，指定同一个调度节点（如 `router-a`）。每个进程同一组只能对应一个配置；原有多个 DNS 配置仍可并存。
5. 在所有成员上设置一致的组成员顺序、调度节点、IP 来源、算法、前缀长度、后缀和域名绑定。执行前会校验配置摘要，不一致会拒绝更新。本地网卡、IP 获取 URL/命令、DNS 凭据可以不同；每个可能的执行节点都需要有效 DNS 配置。
6. 保存后触发一次更新。节点或配置的编辑会进入审计日志；数据库设置在进程重启后生效。

节点的所属组字段可为空（允许被多个配置组关联）；非空时限制只能关联到该组。每项配置可关联多个节点，最多配置 128 个节点。

## IP 来源和选择算法

- **指定节点**：默认本机。使用指定节点的原有接口、网卡或命令结果；该节点不可用时跳过普通更新，不使用其他地址冒充。
- **自动选择存活节点**：从探活成功且返回有效 IP 的节点中随机或轮询选取。
- **IPv6 前缀＋自定义后缀**：从指定来源节点取得地址，以配置组当前前缀拼接自定义后缀，例如 `::1234`。IPv4 仍使用该来源节点的结果。

随机是默认算法，轮询在当前进程内按关联节点顺序轮转，重启后从头开始。`cluster.Selector` 和 `RegisterSelector` 可替换／扩展选择实现。IP 来源选择和 DNS 执行节点选择分别进行；普通更新和前缀批次都仅由一个选中的执行节点执行。

`GET /ping` 无需登录，固定返回 `{"status":"ok"}`，不访问配置或数据库。远程探活最多并发 8 个，每个超时 2 秒。`/cluster/ip` 和 `/cluster/update` 使用独立的 Bearer 令牌和调度节点标识验证，并不公开。

## IPv6 前缀变化

前缀长度默认为 64，可设为 1–128，支持 /57、/73 等非整字节长度。自定义后缀必须仅使用主机位，例如 /64 的 `::1234`。同一组中的地址应属于同一个前缀变化域，不能把不同网络的 IPv6 地址混为同组。

在“集群管理”中为每个需要批量更新的节点填写独立 IPv6 域名（每行一个），可填写固定后缀，也可在节点首次上报时学习后缀。节点域名不能相互重复，也不能与配置表单中的普通 IPv6 域名重叠。普通域名用于所选来源地址；节点域名用于每个节点自己的重组地址。

调度节点轮询各存活成员的 IP。当某成员上报不同的新前缀时，保留各节点已知后缀，以新前缀重组节点地址，然后再次探活并选取一个执行节点完成整个批次。已离线节点只要有已知后缀，也会更新其绑定域名；没有后缀时不猜测地址，任务报错等待补齐。首次获取前缀时也会初始化节点域名。不同节点同时报告相互冲突的新前缀时暂停更新。

状态保存在配置文件旁的 `.cluster-state.json` 文件中。批次先持久化再执行，仅全部成功后确认新前缀。失败或重启后优先重试整个批次，因此提供的是至少一次执行；DNS 写入应是幂等的，Callback 服务也应支持重复调用。不会自动回滚已成功的外部 DNS 更新。

## 审计存储

默认 SQLite 文件为 `<配置路径>.audit.db`。可以在集群页面配置驱动和 DSN，或在 YAML 中设置：

```yaml
audit:
  driver: sqlite
  dsn: /var/lib/ddns-go/audit.db
```

审计模块使用 `database/sql` 和无 CGO SQLite 驱动，不需要 SQLite 外部进程。记录采用 UTC 时间；查询页显示浏览器本地时间，并支持配置名称、节点名称、操作类型过滤和分页。

审计包含配置和节点的添加、编辑、删除，登录、退出、设置变更、节点选择、前缀变化和 DNS 更新结果。密码、令牌、DNS 凭据及可能包含凭据的命令和 URL 不记录原文。DNS 更新的前值为执行进程上次成功应用的值，首次执行标为未知，不宣称它是实时查询到的权威 DNS 值。

写入通过 1024 条有界队列异步处理，数据库写入不阻塞 DNS 业务。数据库失败或队列满时会输出服务日志，并在审计查询页显示未持久化事件计数；异常退出可能丢失尚未刷盘的事件。正常停止最多等待 5 秒刷新。此实现不是零丢失的合规审计系统。SQLite 文件和配置文件应存放在仅服务账户可访问的目录，备份时包含数据库及其 WAL 文件。

`audit.Store` 定义 `Append`、`Query`、`Close`，使用 `audit.Register("mysql", factory)` 或 `audit.Register("postgres", factory)` 注册额外适配器。适配器自行处理数据库驱动、连接、建表和 SQL 方言；当前仅内置 SQLite，不会因填写 MySQL/PostgreSQL DSN 自动安装驱动。

## 验证

```sh
make build test
CGO_ENABLED=1 go test -race ./...
GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOARCH=riscv64 CGO_ENABLED=0 go build ./...
GOARCH=mipsle CGO_ENABLED=0 go build ./...
```

新增测试使用模拟节点和本地模拟 DNS 服务商，覆盖实际远程执行、离线节点、前缀变化、失败批次恢复、旧配置兼容、鉴权、表单序列化和 SQLite 持久化。无需真实 DNS 账户。
