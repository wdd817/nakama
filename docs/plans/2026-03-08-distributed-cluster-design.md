# Nakama 分布式集群设计

## 概述

将 Nakama 从单节点架构扩展为真正的分布式服务，支持水平扩容与高可用。基于 gossip 协议（hashicorp/memberlist）实现去中心化集群，零外部依赖（除数据库外）。

## 设计原则

- 接口兼容：保留现有 `Local*` 实现，新增 `Distributed*` 实现，通过配置切换
- 零外部依赖：gossip 协议嵌入进程内，不引入 NATS/Redis 等中间件
- Match 宕机即丢失：节点故障时 Match 状态不迁移，玩家重新匹配
- 按大规模（20+ 节点）标准设计

## 第一层：集群基础设施（Cluster Layer）

新增 `server/cluster.go`，基于 `hashicorp/memberlist`。

### 职责

- 节点发现与成员管理（join/leave/failure detection）
- 节点元数据广播（名称、负载、端口等）
- 两种消息传递原语：
  - `Broadcast(msg []byte)` — gossip 广播，适合小消息（< 1KB）
  - `SendTo(node string, msg []byte)` — TCP 直连，适合大消息和点对点 RPC
  - `Request(node string, msg []byte) ([]byte, error)` — 基于 SendTo 封装的 Request/Reply

### 消息协议

自定义二进制消息头：`[type:1byte][source_node:variable][payload]`

消息类型：`PresenceEvent`、`MatchRPC`、`MatchmakerSync`、`PartyRPC`、`StatusSync` 等。上层组件通过注册 handler 按类型分发。

### 节点元数据

```go
type NodeMeta struct {
    Name         string
    GrpcPort     int
    SessionCount int32
    MatchCount   int32
}
```

### 节点发现

通过配置种子节点地址（`JoinAddrs`）引导加入。启动时对 JoinAddrs 做 DNS 解析，支持 Kubernetes headless Service 自动发现（DNS A 记录返回所有 Pod IP）。

### Request/Reply 实现

memberlist 原生只有单向 send，Cluster 层封装 Request/Reply：

```go
func (c *Cluster) Request(node string, msg []byte) ([]byte, error) {
    reqID := uuid.New()
    ch := make(chan []byte, 1)
    c.pendingRequests.Store(reqID, ch)
    defer c.pendingRequests.Delete(reqID)
    c.SendTo(node, encodeRequest(reqID, msg))
    select {
    case resp := <-ch:
        return resp, nil
    case <-time.After(c.requestTimeout):
        return nil, ErrRequestTimeout
    }
}
```

## 第二层：分布式 Tracker（Presence）

新增 `server/tracker_distributed.go`，实现 `Tracker` 接口。

### 核心思路

本地权威 + gossip 广播 + 远程缓存。每个节点是自己会话 presence 的权威源，变更通过 gossip 广播，其他节点维护远程缓存。

### 数据结构

```go
type DistributedTracker struct {
    localTracker *LocalTracker
    remotePeers  map[string]*peerState  // node_name -> 远程 presence 快照
    cluster      *Cluster
}

type peerState struct {
    presences map[PresenceStream][]*Presence
    lastSeen  time.Time
}

### 工作流程

- `Track()/Untrack()/Update()` — 操作 localTracker + gossip 广播 PresenceEvent
- `ListByStream()` — 合并本地结果 + 远程缓存
- `Count()` — 本地 + 所有远程节点计数之和
- `ListLocalByStream()` 等本地方法 — 直接委托 localTracker

### gossip 消息格式

```go
type PresenceEvent struct {
    Op        uint8  // Join=0, Leave=1, Update=2
    Stream    PresenceStream
    UserID    uuid.UUID
    SessionID uuid.UUID
    Meta      PresenceMeta
}
```

### 节点离线处理

memberlist 检测到节点离开 → 清除 remotePeers 中该节点所有 presence → 触发 leave 事件通知 Match/Party listener。

### 一致性

最终一致性，gossip 传播延迟通常 < 1s。对 Presence 场景可接受。

## 第三层：分布式 MessageRouter

新增 `server/message_router_distributed.go`，实现 `MessageRouter` 接口。

### 核心思路

本地投递 + 远程转发。根据目标 presence 的 node 字段判断：本地走 localRouter，远程通过 cluster.SendTo 转发。

### 数据结构

```go
type DistributedMessageRouter struct {
    localRouter     *LocalMessageRouter
    sessionRegistry SessionRegistry
    tracker         Tracker
    cluster         *Cluster
    node            string
}
```

### 方法实现

- `SendToPresenceIDs()` — 按 node 分组，本地直接投递，远程按目标节点批量打包转发
- `SendToStream()` — 通过 DistributedTracker 获取全局 presenceID 列表，走 SendToPresenceIDs 逻辑
- `SendDeferred()` — 每条 DeferredMessage 拆分本地/远程分别处理
- `SendToAll()` — 本地 SendToAll + cluster.Broadcast 广播，各节点收到后调本地 SendToAll

### 远程节点收到消息后

通过 cluster handler 解码，调用 localRouter.SendToPresenceIDs 投递给本地 session。

## 第四层：分布式 Matchmaker

新增 `server/matchmaker_distributed.go`，实现 `Matchmaker` 接口。

### 核心思路

本地收集 + 周期同步 + 本地匹配。每个节点独立接收匹配请求，周期性同步 ticket 到集群，每个节点拥有全局视图后独立执行匹配算法。

### 数据结构

```go
type DistributedMatchmaker struct {
    localMatchmaker *LocalMatchmaker
    cluster         *Cluster
    node            string
}
```

### 工作流程

- `Add()` — 写入 localMatchmaker + gossip 广播 MatchmakerAdd 事件
- `RemoveSession()/RemoveParty()` — 本地移除 + 广播 MatchmakerRemove 事件
- 其他节点收到事件后调用 `localMatchmaker.Insert()` / `Remove()`

### 匹配去重

按 ticket 归属节点决定匹配权：匹配结果中创建时间最早的 ticket 所在的原始节点负责产出结果，其他节点丢弃。

```go
func (m *DistributedMatchmaker) isMatchOwner(entries []*MatchmakerEntry) bool {
    earliest := entries[0]
    for _, e := range entries[1:] {
        if e.CreateTime < earliest.CreateTime {
            earliest = e
        }
    }
    return earliest.Presence.Node == m.node
}
```

### 已有接口适配

`Extract()` 和 `Insert()` 方法天然适配跨节点同步。

### 同步策略

- 新 ticket 立即广播
- 每 5-10s 全量 Extract/Insert 对账
- 节点离线时调用 `RemoveAll(node)` 清除该节点 ticket

## 第五层：分布式 MatchRegistry

新增 `server/match_registry_distributed.go`，实现 `MatchRegistry` 接口。

### 核心思路

Match 固定在创建节点运行，跨节点操作通过 RPC 转发。Match ID 格式 `{uuid}.{node}` 天然携带节点信息。

### 数据结构

```go
type DistributedMatchRegistry struct {
    localRegistry *LocalMatchRegistry
    cluster       *Cluster
    node          string
}
```

### 路由逻辑

- `CreateMatch()` — 本地创建 + 广播 MatchIndexUpdate
- `GetMatch()` — 解析 node，本地查 localRegistry，远程 RPC 转发
- `ListMatches()` — 合并本地索引 + 远程索引缓存
- `JoinAttempt()/Join()/Leave()/Kick()/SendData()/Signal()/GetState()` — 按 node 路由，远程 RPC 转发
- `RemoveMatch()` — 本地移除 + 广播 MatchIndexRemove

### 索引同步

- Match 创建/更新/移除时立即广播
- 每 10s 全量对账
- 节点离线时清除该节点所有 Match 索引

## 第六层：分布式 PartyRegistry

新增 `server/party_registry_distributed.go`，实现 `PartyRegistry` 接口。

### 核心思路

与 MatchRegistry 完全一致 — Party 固定在创建节点，跨节点操作 RPC 转发。Party ID 格式也是 `{uuid}.{node}`。

### 数据结构

```go
type DistributedPartyRegistry struct {
    localRegistry *LocalPartyRegistry
    cluster       *Cluster
    node          string
}
```

### 路由逻辑

与 MatchRegistry 同构。所有带 node 参数的方法按 node 路由，本地直接调 localRegistry，远程 RPC 转发。

### 与 MatchRegistry 的复用

抽取通用 `DistributedEntityRouter`：

```go
type DistributedEntityRouter struct {
    cluster *Cluster
    node    string
}

func (r *DistributedEntityRouter) RouteOrLocal(targetNode string, localFn func() ([]byte, error), msgType uint8, payload []byte) ([]byte, error) {
    if targetNode == r.node {
        return localFn()
    }
    return r.cluster.Request(targetNode, encode(msgType, payload))
}
```

## 第七层：SessionRegistry 与 StreamManager

### SessionRegistry — 不需要分布式实现

WebSocket 连接天然绑定本地节点，保持 `LocalSessionRegistry`。跨节点 session 感知由 DistributedTracker 覆盖。

### StreamManager — 薄封装

新增 `server/stream_manager_distributed.go`。底层操作 Tracker，注入 DistributedTracker 后自动获得分布式能力。本地 session 操作委托 localStreamManager，远程 session 通过 cluster.SendTo 转发。

## 第八层：分布式 StatusHandler

新增 `server/status_handler_distributed.go`，实现 `StatusHandler` 接口。

`GetStatus()` — 收集本地状态 + 向所有远程节点发送 Request 获取状态，合并返回。管理控制台可查看整个集群状态。

## 配置

```go
type ClusterConfig struct {
    Enabled                bool
    GossipPort             int           // 默认 7352
    JoinAddrs              []string
    MetaInterval           time.Duration // 默认 5s
    MatchmakerSyncInterval time.Duration // 默认 10s
    MatchIndexSyncInterval time.Duration // 默认 10s
    RequestTimeout         time.Duration // 默认 5s
}
```

## 组装（main.go）

`cluster.Enabled=false` 时使用 Local 实现（零影响），`true` 时初始化 Cluster 并注入 Distributed 实现。现有 API/Console/Runtime 层代码无需改动。

## 端口

| 端口 | 服务 |
|---|---|
| 7349 | gRPC API |
| 7350 | HTTP API + WebSocket |
| 7351 | 管理控制台 |
| 7352 | Gossip 集群通信（新增） |
| 9100 | Prometheus 指标 |

## 新增依赖

- `github.com/hashicorp/memberlist` — gossip 协议实现

## 新增文件清单

| 文件 | 职责 |
|---|---|
| `server/cluster.go` | 集群基础设施（memberlist 封装、消息分发、Request/Reply） |
| `server/tracker_distributed.go` | 分布式 Presence 追踪 |
| `server/message_router_distributed.go` | 分布式消息路由 |
| `server/matchmaker_distributed.go` | 分布式匹配系统 |
| `server/match_registry_distributed.go` | 分布式 Match 注册 |
| `server/party_registry_distributed.go` | 分布式 Party 注册 |
| `server/stream_manager_distributed.go` | 分布式 Stream 管理 |
| `server/status_handler_distributed.go` | 分布式状态查询 |
