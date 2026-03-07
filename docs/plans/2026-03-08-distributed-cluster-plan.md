# Nakama Distributed Cluster Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add gossip-based distributed clustering to Nakama, enabling horizontal scaling and high availability with zero external dependencies beyond the database.

**Architecture:** All core components get Distributed* implementations alongside existing Local* ones. A new Cluster layer based on hashicorp/memberlist provides node discovery, membership, and message passing. Configuration flag cluster.enabled switches between single-node and cluster mode.

**Tech Stack:** Go 1.25, hashicorp/memberlist (gossip), protobuf for message serialization

**Design Doc:** docs/plans/2026-03-08-distributed-cluster-design.md

---

## Task 1: Add memberlist dependency

**Files:**
- Modify: `go.mod`
- Modify: `vendor/` (via go mod vendor)

**Step 1: Add memberlist dependency**

Run: `go get github.com/hashicorp/memberlist`

**Step 2: Vendor the dependency**

Run: `go mod vendor`

**Step 3: Verify build**

Run: `go build -trimpath -mod=vendor ./...`
Expected: BUILD SUCCESS

**Step 4: Commit**

```bash
git add go.mod go.sum vendor/
git commit -m "deps: add hashicorp/memberlist for cluster support"
```

---

## Task 2: Cluster configuration

**Files:**
- Modify: `server/config.go` (~line 223, after existing config structs)
- Create: `server/config_cluster_test.go`

**Step 1: Write the failing test**

Create `server/config_cluster_test.go`:

```go
package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClusterConfigDefaults(t *testing.T) {
	cfg := NewClusterConfig()
	assert.False(t, cfg.Enabled)
	assert.Equal(t, 7352, cfg.GossipPort)
	assert.Empty(t, cfg.JoinAddrs)
	assert.Equal(t, 5*time.Second, cfg.MetaInterval)
	assert.Equal(t, 10*time.Second, cfg.MatchmakerSyncInterval)
	assert.Equal(t, 10*time.Second, cfg.MatchIndexSyncInterval)
	assert.Equal(t, 5*time.Second, cfg.RequestTimeout)
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v -run TestClusterConfigDefaults ./server/`
Expected: FAIL - "undefined: NewClusterConfig"

**Step 3: Write minimal implementation**

Add to `server/config.go` after existing config structs:

```go
type ClusterConfig struct {
	Enabled                bool          `yaml:"enabled" json:"enabled" usage:"Enable cluster mode."`
	GossipPort             int           `yaml:"gossip_port" json:"gossip_port" usage:"Port for gossip protocol."`
	JoinAddrs              []string      `yaml:"join_addrs" json:"join_addrs" usage:"Seed node addresses."`
	MetaInterval           time.Duration `yaml:"meta_interval" json:"meta_interval" usage:"Node metadata broadcast interval."`
	MatchmakerSyncInterval time.Duration `yaml:"matchmaker_sync_interval" json:"matchmaker_sync_interval" usage:"Matchmaker full sync interval."`
	MatchIndexSyncInterval time.Duration `yaml:"match_index_sync_interval" json:"match_index_sync_interval" usage:"Match/Party index sync inn	RequestTimeout         time.Duration `yaml:"request_timeout" json:"request_timeout" usage:"Cross-node RPC timeout."`
}

func NewClusterConfig() *ClusterConfig {
	return &ClusterConfig{
		Enabled:                false,
		GossipPort:             7352,
		JoinAddrs:              []string{},
		MetaInterval:           5 * time.Second,
		MatchmakerSyncInterval: 10 * time.Second,
		MatchIndexSyncInterval: 10 * time.Second,
		RequestTimeout:         5 * time.Second,
	}
}
```

Also add `Cluster *ClusterConfig` field to the main config struct and `GetCluster() *ClusterConfig` to the Config interface. Initialize in `NewConfig()`.

**Step 4: Run test to verify it passes**

Run: `go test -v -run TestClusterConfigDefaults ./server/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/config.go server/config_cluster_test.go
git commit -m "feat(cluster): add ClusterConfig with defaults"
```

---

## Task 3: Cluster message protocol

**Files:**
- Create: `server/cluster_msg.go`
- Create: `server/cluster_msg_test.go`

**Step 1: Write the failing test**

Create `server/cluster_msg_test.go` with tests for encode/decode of ClusterMessage and Request/Response payloads.

Key test cases:
- `TestClusterMessageEncodeDecode` - round-trip encode/decode of ClusterMessage struct
- `TestClusterRequestEncodeDecode` - round-trip encode/decode of request ID + payload

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestClusterMessage|TestClusterRequest" ./server/`
Expected: FAIL - "undefined: ClusterMessage"

**Step 3: Write minimal implementation**

Create `server/cluster_msg.go` with:

```go
type ClusterMessageType uint8

const (
	ClusterMessageType_PresenceEvent ClusterMessageType = iota + 1
	ClusterMessageType_MatchRPC
	ClusterMessageType_MatchmakerSync
	ClusterMessageType_MatchmakerRemove
	ClusterMessageType_PartyRPC
	ClusterMessageType_MatchIndexSync
	ClusterMessageType_PartyIndexSync
	ClusterMessageType_StatusRequest
	ClusterMessageType_Envelope
	ClusterMessageType_EnvelopeBroadcast
	ClusterMessageType_StreamRPC
	ClusterMessageType_Request
	ClusterMessageType_Response
)

type ClusterMessage struct {
	Type       ClusterMessageType
	SourceNode string
	Payload    []byte
}
```

Binary wire format: `[type:1byte][node_len:1byte][node:variable][payload:rest]`

Request wire format: `[reqID:16bytes][payload:rest]`

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestClusterMessage|TestClusterRequest" ./server/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/cluster_msg.go server/cluster_msg_test.go
git commit -m "feat(cluster): add message protocol encode/decode"
```

---

## Task 4: Cluster layer - core

**Files:**
- Create: `server/cluster.go`
- Create: `server/cluster_test.go`

**Step 1: Write the failing tests**

Create `server/cluster_test.go` with 4 test cases:

- `TestClusterTwoNodeJoin` - Two nodes discover each other via gossip, verify MemberCount and MemberNames
- `TestClusterSendTo` - Node1 sends a message to Node2 via TCP direct, Node2 receives via registered handler
- `TestClusterRequestReply` - Node2 sends Request to Node1, Node1 replies, Node2 gets response
- `TestClusterNodeLeaveCallback` - Node2 stops, Node1 receives OnNodeLeave callback

Use `GossipPort: 0` for random port assignment in tests. Use `c1.LocalAddr()` as JoinAddrs for c2.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestCluster" ./server/`
Expected: FAIL - "undefined: NewCluster"

**Step 3: Write minimal implementation**

Create `server/cluster.go` with:

```go
type Cluster struct {
    logger          *zap.Logger
    node            string
    config          *ClusterConfig
    ml              *memberlist.Memberlist
    handlers        map[ClusterMessageType]ClusterMessageHandler
    handlersMu      sync.RWMutex
    pendingRequests sync.Map // uuid.UUID -> chan []byte
    onNodeLeave     func(string)
    onNodeLeaveMu   sync.RWMutex
}
```

Key methods:
- `NewCluster(logger, nodeName, config)` - creates memberlist, joins seed nodes (with DNS resolution)
- `Stop()` - graceful leave + shutdown
- `LocalAddr() string` - returns gossip bind address
- `MemberCount() int` / `MemberNames() []string`
- `RegisterHandler(msgType, handler)` - register message handler by type
- `OnNodeLeave(fn)` - register node leave callback
- `SendTo(nodeName, msgType, payload)` - TCP direct send to specific node
- `Broadcast(msgType, payload)` - send to all other nodes via TCP reliable
- `Request(nodeName, msgType, payload) ([]byte, error)` - request/reply with timeout
- `Reply(msg, response)` - send response back to requester

Internal:
- `clusterDelegate` implements memberlist.Delegate (NotifyMsg dispatches to handlers)
- `clusterEventDelegate` implements memberlist.EventDelegate (NotifyLeave triggers callback)
- `resolveJoinAddrs(addrs)` - DNS resolution for headless Service support
- `dispatchMessage(msg)` - routes to handler by type, handles Request/Response specially

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestCluster" ./server/`
Expected: All 4 tests PASS

**Step 5: Commit**

```bash
git add server/cluster.go server/cluster_test.go
git commit -m "feat(cluster): add core cluster layer with memberlist"
```

---

## Task 5: Distributed Tracker

**Files:**
- Create: `server/tracker_distributed.go`
- Create: `server/tracker_distributed_test.go`

**Context:** Tracker interface defined at `server/tracker.go:77-104`. LocalTracker at `server/tracker.go:106-127`. PresenceID (line 45), PresenceStream (line 50), PresenceMeta (line 57), Presence (line 66).

**Step 1: Write the failing tests**

Create `server/tracker_distributed_test.go` with test cases:

- `TestDistributedTrackerTrackBroadcasts` - Track a presence on node1, verify gossip broadcast is sent with PresenceEvent
- `TestDistributedTrackerRemotePresence` - Simulate receiving a remote PresenceEvent (join), verify ListByStream returns both local and remote presences
- `TestDistributedTrackerUntrackBroadcasts` - Untrack a presence, verify leave event is broadcast
- `TestDistributedTrackerNodeLeave` - Simulate node leave, verify all remote presences from that node are cleared
- `TestDistributedTrackerCountIncludesRemote` - Count() returns local + remote totals
- `TestDistributedTrackerListLocalOnly` - ListLocalByStream only returns local presences, not remote

Use mock Cluster that captures broadcast/send calls instead of real memberlist.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedTracker" ./server/`
Expected: FAIL - "undefined: DistributedTracker"

**Step 3: Write minimal implementation**

Create `server/tracker_distributed.go`:

```go
type DistributedTracker struct {
    localTracker *LocalTracker
    remotePeers  map[string]*peerState  // node_name -> remote presences
    remoteMu     sync.RWMutex
    cluster      *Cluster
    node         string
    logger       *zap.Logger
}

type peerState struct {
    presences map[PresenceStream]map[presenceKey]*Presence
}

type presenceKey struct {
    SessionID uuid.UUID
    UserID    uuid.UUID
}
```

Implementation approach:
- All mutating methods (Track/Untrack/Update) delegate to localTracker first, then broadcast PresenceEvent via cluster.Broadcast
- All read methods (ListByStream/Count) merge local results with remotePeers cache
- All "Local" read methods (ListLocalByStream/ListLocalSessionIDByStream/GetLocalBySessionIDStreamUserID) delegate directly to localTracker
- Register cluster handler for ClusterMessageType_PresenceEvent to update remotePeers cache
- Register OnNodeLeave callback to clear remotePeers[deadNode] and trigger match/party leave listeners
- Constructor: `NewDistributedTracker(logger, localTracker, cluster, node)`

PresenceEvent encode/decode: use encoding/binary for fixed fields, JSON or custom binary for PresenceStream and PresenceMeta.

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedTracker" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/tracker_distributed.go server/tracker_distributed_test.go
git commit -m "feat(cluster): add DistributedTracker with gossip presence sync"
```

---

## Task 6: Distributed MessageRouter

**Files:**
- Create: `server/message_router_distributed.go`
- Create: `server/message_router_distributed_test.go`

**Context:** MessageRouter interface at `server/message_router.go:33-38`. LocalMessageRouter at `server/message_router.go:40-44`. DeferredMessage at line 26.

**Step 1: Write the failing tests**

Create `server/message_router_distributed_test.go` with test cases:

- `TestDistributedRouterLocalDelivery` - SendToPresenceIDs with local presences only, verify localRouter receives them
- `TestDistributedRouterRemoteForward` - SendToPresenceIDs with remote presences, verify cluster.SendTo is called with correct target node
- `TestDistributedRouterMixedDelivery` - Mix of local and remote presences, verify local delivered locally and remote forwarded
- `TestDistributedRouterSendToStream` - SendToStream resolves presences via tracker, routes correctly
- `TestDistributedRouterSendToAll` - SendToAll delivers locally and broadcasts to all nodes
- `TestDistributedRouterReceiveRemoteEnvelope` - Simulate receiving a remote envelope message, verify local delivery

Use mock Cluster and mock LocalMessageRouter.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedRouter" ./server/`
Expected: FAIL - "undefined: DistributedMessageRouter"

**Step 3: Write minimal implementation**

Create `server/message_router_distributed.go`:

```go
type DistributedMessageRouter struct {
    localRouter     *LocalMessageRouter
    sessionRegistry SessionRegistry
    tracker         Tracker
    cluster         *Cluster
    node            string
    logger          *zap.Logger
}

func NewDistributedMessageRouter(logger *zap.Logger, localRouter *LocalMessageRouter, sessionRegistry SessionRegistry, tracker Tracker, cluster *Cluster, node string) *DistributedMessageRouter
```

Implementation:
- `SendToPresenceIDs` - partition by node field, local goes to localRouter, remote grouped by node and sent via cluster.SendTo(node, ClusterMessageType_Envelope, encodedPayload)
- `SendToStream` - get all presenceIDs from tracker.ListPresenceIDByStream, then call SendToPresenceIDs
- `SendDeferred` - iterate DeferredMessages, each one goes through SendToPresenceIDs
- `SendToAll` - localRouter.SendToAll + cluster.Broadcast(ClusterMessageType_EnvelopeBroadcast, encodedEnvelope)
- Register cluster handler for ClusterMessageType_Envelope: decode presenceIDs + envelope, call localRouter.SendToPresenceIDs
- Register cluster handler for ClusterMessageType_EnvelopeBroadcast: decode envelope, call localRouter.SendToAll

Envelope wire format: protobuf marshal the rtapi.Envelope, prepend presence ID list.

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedRouter" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/message_router_distributed.go server/message_router_distributed_test.go
git commit -m "feat(cluster): add DistributedMessageRouter with remote forwarding"
```

---

## Task 7: Distributed Matchmaker

**Files:**
- Create: `server/matchmaker_distributed.go`
- Create: `server/matchmaker_distributed_test.go`

**Context:** Matchmaker interface at `server/matchmaker.go:175-192`. LocalMatchmaker at `server/matchmaker.go:240-273`. Key methods: Add, Insert, Extract, RemoveSession, RemoveParty, RemoveAll, Remove. MatchmakerPresence (line 34), MatchmakerEntry (line 67), MatchmakerExtract (line 116).

**Step 1: Write the failing tests**

Create `server/matchmaker_distributed_test.go` with test cases:

- `TestDistributedMatchmakerAddBroadcasts` - Add a ticket, verify MatchmakerSync broadcast is sent
- `TestDistributedMatchmakerReceiveRemoteTicket` - Simulate receiving remote MatchmakerSync, verify localMatchmaker.Insert is called
- `TestDistributedMatchmakerRemoveBroadcasts` - RemoveSession, verify MatchmakerRemove broadcast is sent
- `TestDistributedMatchmakerNodeLeaveCleanup` - Simulate node leave, verify RemoveAll(deadNode) is called
- `TestDistributedMatchmakerDedup` - isMatchOwner returns true only for the node owning the earliest ticket
- `TestDistributedMatchmakerPeriodicSync` - Verify periodic Extract/broadcast runs at configured interval

Use mock Cluster and mock LocalMatchmaker.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedMatchmaker" ./server/`
Expected: FAIL - "undefined: DistributedMatchmaker"

**Step 3: Write minimal implementation**

Create `server/matchmaker_distributed.go`:

```go
type DistributedMatchmaker struct {
    localMatchmaker *LocalMatchmaker
    cluster         *Cluster
    node            string
    logger          *zap.Logger
    ctx             context.Context
    ctxCancelFn     context.CancelFunc
}

func NewDistributedMatchmaker(logger *zap.Logger, localMatchmaker *LocalMatchmaker, cluster *Cluster, node string, syncInterval time.Duration) *DistributedMatchmaker
```

Implementation:
- `Add()` - delegate to localMatchmaker.Add, then broadcast MatchmakerSync with the new ticket data
- `Insert()` - delegate to localMatchmaker.Insert (used for receiving remote tickets)
- `Extract()` - delegate to localMatchmaker.Extract (used for periodic sync)
- `RemoveSession/RemoveParty` - delegate to local, then broadcast MatchmakerRemove
- `RemoveAll(node)` - delegate to localMatchmaker.RemoveAll
- `Remove(tickets)` - delegate to local, then broadcast removal
- `OnMatchedEntries(fn)` - wrap fn with isMatchOwner filter, then delegate to localMatchmaker
- Background goroutine: every syncInterval, call Extract() and broadcast all local tickets for full reconciliation
- Register cluster handler for MatchmakerSync: decode and call localMatchmaker.Inserr cluster handler for MatchmakerRemove: decode and call localMatchmaker.Remove
- Register OnNodeLeave: call localMatchmaker.RemoveAll(deadNode)

Dedup via isMatchOwner: wrap the OnMatchedEntries callback to filter out matches where this node is not the owner.

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedMatchmaker" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/matchmaker_distributed.go server/matchmaker_distributed_test.go
git commit -m "feat(cluster): add DistributedMatchmaker with cross-node ticket sync"
```

---

## Task 8: Distributed MatchRegistry

**Files:**
- Create: `server/match_registry_distributed.go`
- Create: `server/match_registry_distributed_test.go`

**Context:** MatchRegistry interface at `server/match_registry.go:87-126`. LocalMatchRegistry at `server/match_registry.go:128-149`. MatchIndexEntry (line 60). Match ID format: `{uuid}.{node}`.

**Step 1: Write the failing tests**

Create `server/match_registry_distributed_test.go` with test cases:

- `TestDistributedMatchRegistryCreateBroadcastsIndex` - CreateMatch on local node, verify MatchIndexSync broadcast is sent
- `TestDistributedMatchRegistryGetMatchLocal` - GetMatch for local match ID, verify delegates to localRegistry
- `TestDistributedMatchRegistryGetMatchRemote` - GetMatch for remote match ID (uuid.node2), verify cluster.Request is sent to node2
- `TestDistributedMatchRegistryListMatchesMerged` - ListMatches returns local matches + cached remote index entries
- `TestDistributedMatchRegistryJoinAttemptRouting` - JoinAttempt routes to correct node based on match ID
- `TestDistributedMatchRegistrySendDataRouting` - SendData routes to correct node
- `TestDistributedMatchRegistryNodeLeaveCleanup` - Node leave clears that node's index cache
- `TestDistributedMatchRegistryPeriodicIndexSync` - Periodic full index reconciliation

Use mock Cluster and mock LocalMatchRegistry.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedMatchRegistry" ./server/`
Expected: FAIL - "undefined: DistributedMatchRegistry"

**Step 3: Write minimal implementation**

Create `server/match_registry_distributed.go`:

```go
type DistributedMatchRegistry struct {
    localRegistry *LocalMatchRegistry
    remoteIndex   map[string][]*MatchIndexEntry  // node -> entries
    remoteIndexMu sync.RWMutex
    cluster       *Cluster
    node          string
    logger        *zap.Logger
    ctx           context.Context
    ctxCancelFn   context.CancelFunc
}

func NewDistributedMatchRegistry(logger *zap.Logger, localRegistry *LocalMatchRegistry, cluster *Cluster, node string, syncInterval time.Duration) *DistributedMatchRegistry
```

Implementation:
- Helper: `parseMatchNode(matchID string) (uuid.UUID, string)` - split match ID into uuid and node
- `CreateMatch()` - delegate to localRegistry, then broadcast MatchIndexSync with new entry
- `GetMatch()` - parse node from ID, local -> localRegite -> cluster.Request
- `ListMatches()` - query localRegistry + merge remoteIndex cache, apply filters
- `JoinAttempt/Join/Leave/Kick/SendData/Signal/GetState` - parse node, local -> localRegistry, remote -> cluster.Request or cluster.SendTo (fire-and-forget for Join/Leave/Kick)
- `RemoveMatch()` - delegate to local, broadcast MatchIndexRemove
- `UpdateMatchLabel/ClearMatchLabel` - delegate to local, broadcast index update
- `Stop()` - delegate to localRegistry.Stop, cancel background goroutine
- `Count()` - local count + sum of remote index entry counts
- Background goroutine: every syncInterval, broadcast full local index for reconciliation
- Register cluster handlers for MatchIndexSync (update cache) and MatchRPC (dispatch to localRegistry methods)
- Register OnNodeLeave: clear remoteIndex[deadNode]

RPC encoding: use a sub-type byte to distinguish JoinAttempt/SendData/Signal/GetState within MatchRPC messages.

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedMatchRegistry" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/match_registry_distributed.go server/match_registry_distributed_test.go
git commit -m "feat(cluster): add DistributedMatchRegistry with cross-node RPC routing"
```

---

## Task 9: Distributed PartyRegistry

**Files:**
- Create: `server/party_registry_distributed.go`
- Create: `server/party_registry_distributed_test.go`

**Context:** PartyRegistry interface at `server/party_registry.go:49-71`. LocalPartyRegistry at `server/party_registry.go:73-93`. PartyIndexEntry (line 95). Party ID format: `{uuid}.{node}`. Pattern identical to MatchRegistry.

**Step 1: Write the failing tests**

Create `server/party_registry_distributed_test.go` with test cases:

- `TestDistributedPartyRegistryCreateBroadcastsIndex` - Create party, verify PartyIndexSync broadcast
- `TestDistributedPartyRegistryJoinRequestRouting` - PartyJoinRequest routes to correct node based on party ID
- `TestDistributedPartyRegistryPromoteRouting` - PartyPromote routes to party owner node
- `TestDistributedPartyRegistryDataSendRouting` - PartyDataSend routes to party owner node
- `TestDistributedPartyRegistryListMerged` - PartyList returns local + cached remote entries
- `TestDistributedPartyRegistryNodeLeaveCleanup` - Node leave clears remote party index

Use mock Cluster and mock LocalPartyRegistry.

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedPartyRegistry" ./server/`
Expected: FAIL - "undefined: DistributedPartyRegistry"

**Step 3: Write minimal implementation**

Create `server/party_registry_distributed.go`:

```go
type DistributedPartyRegistry struct {
    localRegistry *LocalPartyRegistry
    remoteIndex   map[string][]*PartyIndexEntry  // node -> entries
    remoteIndexMu sync.RWMutex
    cluster       *Cluster
    node          string
    logger        *zap.Logger
    ctx           context.Context
    ctxCancelFn   context.CancelFunc
}

func NewDistributedPartyRegistry(logger *zap.Logger, localRegistry *LocalPartyRegistry, cluster *Cluster, node string, syncInterval time.Duration) *DistributedPartyRegistry
```

Implementation pattern identical to DistributedMatchRegistry:
- `Create()` - local + broadcast PartyIndexSync
- `PartyJoinRequest/PartyPromote/PartyAccept/PartyRemove/PartyClose/PartyJoinRequestList/PartyMatchmakerAdd/PartyMatchmakerRemove/PartyDataSend/PartyUpdate` - parse node from party ID, local -> localRegistry, remote -> cluster.Request
- `PartyList()` - merge local + remote index cache
- `Delete()` - local + broadcast PartyIndexRemove
- `LabelUpdate()` - local + broadcast index update
- `Count()` - local + remote sum
- `Init()` - delegate to localRegistry.Init
- Background goroutine: periodic full index sync
- Register cluster handlers for PartyIndexSync and PartyRPC
- Register OnNodeLeave: clear remoteIndex[deadNode]

Consider extracting shared routing logic into a helper used by both DistributedMatchRegistry and DistributedPartyRegistry (DistributedEntityRouter from design doc).

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedPartyRegistry" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/party_registry_distributed.go server/party_registry_distributed_test.go
git commit -m "feat(cluster): add DistributedPartyRegistry with cross-node RPC routing"
```

---

## Task 10: Distributed StreamManager and StatusHandler

**Files:**
- Create: `server/stream_manager_distributed.go`
- Create: `server/stream_manager_distributed_test.go`
- Create: `server/status_handler_distributed.go`
- Create: `server/status_handler_distributed_test.go`

**Context:** StreamManager interface at `server/stream_manager.go:29-33`. StatusHandler interface at `server/status_handler.go:28-30`.

**Step 1: Write the failing tests**

Create `server/stream_manager_distributed_test.go`:

- `TestDistributedStreamManagerLocalJoin` - UserJoin for local session delegates to localStreamManager
- `TestDistributedStreamManagerRemoteJoin` - UserJoin for remote session forwards via cluster.SendTo

Create `server/status_handler_distributed_test.go`:

- `TestDistributedStatusHandlerLocal` - GetStatus with no remote nodes returns local status only
- `TestDistributedStatusHandlerMergesRemote` - GetStatus queries all cluster members, merges results

**Step 2: Run test to verify it fails**

Run: `go test -v -run "TestDistributedStream|TestDistributedStatus" ./server/`
Expected: FAIL - undefined types

**Step 3: Write minimal implementation**

Create `server/stream_manager_distributed.go`:

```go
type DistributedStreamManager struct {
    localStreamManager *LocalStreamManager
    tracker            Tracker
    cluster            *Cluster
    node               string
    logger             *zap.Logger
}

func NewDistributedStreamManager(logger *zap.Logger, localStreamManager *LocalStreamManager, tracker Tracker, cluster *Cluster, node string) *DistributedStreamManager
```

- `UserJoin/UserUpdate/UserLeave` - check if session is local (via sessionRegistry), if local delegate to localStreamManager, if remote forward via cluster.SendTo(targetNode, ClusterMessageType_StreamRPC, payload)
- Register cluster handler for StreamRPC: decode and call localStreamManager methods

Create `server/status_handler_distributed.go`:

```go
type DistributedStatusHandler struct {
    localHandler *LocalStatusHandler
    cluster      *Cluster
    logger       *zap.Logger
}

func NewDistributedStatusHandler(logger *zap.Logger, localHandler *LocalStatusHandler, cluster *Cluster) *DistributedStatusHandler
```

- `GetStatus()` - get local status, then cluster.Request to each member for their status, merge all results. Skip unreachable nodes with warning log.
- Register cluster handler for StatusRequest: call localHandler.GetStatus, encode and reply

**Step 4: Run test to verify it passes**

Run: `go test -v -run "TestDistributedStream|TestDistributedStatus" ./server/`
Expected: All tests PASS

**Step 5: Commit**

```bash
git add server/stream_manager_distributed.go server/stream_manager_distributed_test.go server/status_handler_distributed.go server/status_handler_distributed_test.go
git commit -m "feat(cluster): add DistributedStreamManager and DistributedStatusHandler"
```

---

## Task 11: Assembly in main.go

**Files:**
- Modify: `main.go` (~lines 180-222, component initialization section)

**Context:** Current initialization at `main.go:181-222` creates all Local* implementations and wires them together. We need to add a conditional branch that creates Distributed* implementations when cluster.enabled=true.

**Step 1: Review current initialization**

Read `main.go:180-222` to confirm exact variable names and initialization order.

**Step 2: Add cluster initialization**

After existing Local* initialization (~line 187), add:

```go
var cluster *server.Cluster

if config.GetCluster().Enabled {
    var err error
    cluster, err = server.NewCluster(logger, config.GetName(), config.GetCluster())
    if err != nil {
        startupLogger.Fatal("Failed to start cluster", zap.Error(err))
    }
    defer cluster.Stop()
}
```

**Step 3: Add conditional Distributed* wiring**

After all Local* components are created, add the conditional switch:

```go
var (
    activeTracker       server.Tracker
    activeRouter        server.MessageRouter
    activeMatchmaker    server.Matchmaker
    activeMatchRegistry server.MatchRegistry
    activePartyRegistry server.PartyRegistry
    activeStreamManager server.StreamManager
    activeStatusHandler server.StatusHandler
)

if cluster != nil {
    activeTracker = server.NewDistributedTracker(logger, tracker, cluster, config.GetName())
    activeRouter = server.NewDistributedMessageRouter(logger, router, sessionRegistry, activeTracker, cluster, config.GetName())
    activeMatchRegistry = server.NewDistributedMatchRegistry(logger, matchRegistry, cluster, config.GetName(), config.GetCluster().MatchIndexSyncInterval)
    activeStreamManager = server.NewDistributedStreamManager(logger, streamManager, activeTracker, cluster, config.GetName())

    // These need runtime, so set up after runtime init
    // activeMatchmaker and activePartyRegistry initialized after runtime

    activeTracker.SetMatchJoinListener(activeMatchRegistry.Join)
    activeTracker.SetMatchLeaveListener(activeMatchRegistry.Leave)
} else {
    activeTracker = tracker
    activeRouter = router
    activeMatchRegistry = matchRegistry
    activeStreamManager = streamManager
}
```

After runtime initialization:

```go
if cluster != nil {
    activeMatchmaker = server.NewDistributedMatchmaker(logger, matchmaker, cluster, config.GetName(), config.GetCluster().MatchmakerSyncInterval)
    activePartyRegistry = server.NewDistributedPartyRegistry(logger, partyRegistry, cluster, config.GetName(), config.GetCluster().MatchIndexSyncInterval)
    activePartyRegistry.Init(activeMatchmaker, activeTracker, activeStreamManager, activeRouter)
    activeTracker.SetPartyJoinListener(activePartyRegistry.Join)
    activeTracker.SetPartyLeaveListener(activePartyRegistry.Leave)
    activeStatusHandler = server.NewDistributedStatusHandler(logger, statusHandler, cluster)
} else {
    activeMatchmaker = matchmaker
  tyRegistry = partyRegistry
    activeStatusHandler = statusHandler
}
```

Replace all downstream usages of tracker/router/matchmaker/matchRegistry/partyRegistry/streamManager/statusHandler with the active* variables.

**Step 4: Verify build**

Run: `go build -trimpath -mod=vendor`
Expected: BUILD SUCCESS

**Step 5: Commit**

```bash
git add main.go
git commit -m "feat(cluster): wire Distributed* components in main.go with config switch"
```

---

## Task 12: Integration test - two-node cluster

**Files:**
- Create: `server/cluster_integration_test.go`

**Step 1: Write integration test**

Create `server/cluster_integration_test.go` with an end-to-end test that spins up two in-process clusters and verifies cross-node behavior:

```go
func TestClusterIntegration_PresenceSync(t *testing.T) {
    // 1. Create two Cluster instances (random ports)
    // 2. Create LocalTracker + DistributedTracker on each
    // 3. Track a presence on node1
    // 4. Wait for gossip propagation (~500ms)
    // 5. Verify node2 sees the presence via ListByStream
    // 6. Untrack on node1
    // 7. Verify node2 no longer sees it
}

func TestClusterIntegration_MessageRouting(t *testing.T) {
    // 1. Create two Cluster instances
    // 2. Create LocalMessageRouter + DistributedMessageRouter on each
    // 3. Create a mock session on node2
    // 4. Track presence for that session on node2 (propagates to node1)
    // 5. From node1, SendToPresenceIDs targeting the node2 session
    // 6. Verify the mock session on node2 received the envelope
}

func TestClusterIntegration_MatchRegistryRouting(t *testing.T) {
    // 1. Create two Cluster instances with full component stack
    // 2. Create a match on node1
    // 3. From node2, call GetMatch with the match ID
    // 4. Verify node2 gets the match info via RPC to node1
    // 5. From node2, call JoinAttempt
    // 6. Verify it routes to node1 and returns result
}

func TestClusterIntegration_NodeFailure(t *testing.T) {
    // 1. Create two Cluster instances with full component stack
    // 2. Track presences and create matches on node2
    // 3. Stop node2
    // 4. Verify node1 clears all remote presences from node2
    // 5. Verify node1 clears all remote match/party index entries from node2
    // 6. Verify matchmaker removes all tickets from node2
}
```

**Step 2: Run integration tests**

Run: `go test -v -run "TestClusterIntegration" ./server/`
Expected: All tests PASS

**Step 3: Commit**

```bash
git add server/cluster_integration_test.go
git commit -m "test(cluster): add integration tests for two-node cluster"
```

---

## Task 13: Docker Compose for cluster development

**Files:**
- Create: `docker-compose-cluster.yml`

**Step 1: Create cluster docker-compose**

Create `docker-compose-cluster.yml` with 3 Nakama nodes + 1 CockroachDB:

```yaml
version: "3"
services:
  cockroachdb:
    image: cockroachdb/cockroach:latest-v23.1
    command: start-single-node --insecure --store=attrs=ssd,path=/var/lib/cockroach/
    ports:
      - "26257:26257"
      - "8080:8080"

  nakama1:
    build: .
    entrypoint:
      - "/bin/sh"
      - "-ecx"
      - >
        /nakama/nakama migrate up --database.address root@cockroachdb:26257 &&
        exec /nakama/nakama
        --name nakama1
        --database.address root@cockroachdb:26257
        --cluster.enabled=true
        --cluster.gossip_port=7352
    ports:
      - "7349:7349"
      - "7350:7350"
      - "7351:7351"
    depends_on:
      - cockroachdb

  nakama2:
    build: .
    entrypoint:
      - "/bin/sh"
      - "-ecx"
      - >
        exec /nakama/nakama
        --name nakama2
        --database.address root@cockroachdb:26257
        --cluster.enabled=true
        --cluster.gossip_port=7352
        --cluster.join_addrs=nakama1:7352
    ports:
      - "7359:7349"
      - "7360:7350"
      - "7361:7351"
    depends_on:
      - nakama1

  nakama3:
    build: .
    entrypoint:
      - "/bin/sh"
      - "-ecx"
      - >
        exec /nakama/nakama
        --name nakama3
        --database.address root@cockroachdb:26257
        --cluster.enabled=true
        --cluster.gossip_port=7352
        --cluster.join_addrs=nakama1:7352
    ports:
      - "7369:7349"
      - "7370:7350"
      - "7371:7351"
    depends_on:
      - nakama1
```

**Step 2: Verify it builds**

Run: `docker-compose -f docker-compose-cluster.yml build`
Expected: BUILD SUCCESS

**Step 3: Commit**

```bash
git add docker-compose-cluster.yml
git commit -m "feat(cluster): add docker-compose for 3-node cluster development"
```

---

## Task Summary

| Task | Component | New Files |
|------|-----------|-----------|
| 1 | memberlist dependency | go.mod, vendor/ |
| 2 | ClusterConfig | server/config.go (modify), server/config_cluster_test.go |
| 3 | Message protocol | server/cluster_msg.go, server/cluster_msg_test.go |
| 4 | Cluster core | server/cluster.go, server/cluster_test.go |
| 5 | DistributedTracker | server/tracker_distributed.go, server/tracker_distributed_test.go |
| 6 | DistributedMessageRouter | server/message_router_distributed.go, server/message_router_distributed_test.go |
| 7 | DistributedMatchmaker | server/matchmaker_distributed.go, server/matchmaker_distributed_test.go |
| 8 | DistributedMatchRegistry | server/match_registry_distributed.go, server/match_registry_distributed_test.go |
| 9 | DistributedPartyRegistry | server/party_registry_distributed.go, server/party_registry_distributed_test.go |
| 10 | DistributedStreamManager + StatusHandler | 4 files (impl + test each) |
| 11 | main.go assembly | main.go (modify) |
| 12 | Integration tests | server/cluster_integration_test.go |
| 13 | Docker Compose cluster | docker-compose-cluster.yml |

**Dependency order:** Task 1 -> 2 -> 3 -> 4 -> 5 -> 6 -> 7,8,9 (parallel) -> 10 -> 11 -> 12 -> 13

Tasks 7, 8, 9 can be implemented in parallel as they are independent of each other (all depend on Cluster layer from Task 4).
