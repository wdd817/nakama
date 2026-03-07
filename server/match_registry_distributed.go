package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type distributedMatchIndexEntry struct {
	MatchID     string `json:"match_id"`
	Node        string `json:"node"`
	HandlerName string `json:"handler_name,omitempty"`
	Label       string `json:"label,omitempty"`
	TickRate    int    `json:"tick_rate,omitempty"`
	CreateTime  int64  `json:"create_time,omitempty"`
	Size        int32  `json:"size,omitempty"`
}

type DistributedMatchRegistry struct {
	localRegistry MatchRegistry
	remoteIndex   map[string][]*distributedMatchIndexEntry // node -> entries
	remoteIndexMu sync.RWMutex
	cluster       ClusterClient
	node          string
	logger        *zap.Logger
}

func NewDistributedMatchRegistry(logger *zap.Logger, localRegistry MatchRegistry, cluster ClusterClient, node string) *DistributedMatchRegistry {
	dm := &DistributedMatchRegistry{
		localRegistry: localRegistry,
		remoteIndex:   make(map[string][]*distributedMatchIndexEntry),
		cluster:       cluster,
		node:          node,
		logger:        logger,
	}

	cluster.RegisterHandler(ClusterMessageType_MatchIndexSync, dm.handleIndexSync)
	cluster.RegisterHandler(ClusterMessageType_MatchRPC, dm.handleMatchRPC)
	cluster.OnNodeLeave(dm.handleNodeLeave)

	return dm
}

func (r *DistributedMatchRegistry) parseMatchNode(matchID string) (string, string) {
	idx := strings.LastIndex(matchID, ".")
	if idx < 0 {
		return matchID, ""
	}
	return matchID[:idx], matchID[idx+1:]
}

func (r *DistributedMatchRegistry) CreateMatch(ctx context.Context, createFn RuntimeMatchCreateFunction, module string, params map[string]interface{}) (string, error) {
	matchID, err := r.localRegistry.CreateMatch(ctx, createFn, module, params)
	if err != nil {
		return "", err
	}

	entry := &distributedMatchIndexEntry{
		MatchID:     matchID,
		Node:        r.node,
		HandlerName: module,
	}
	payload, _ := json.Marshal([]*distributedMatchIndexEntry{entry})
	r.cluster.Broadcast(ClusterMessageType_MatchIndexSync, payload)

	return matchID, nil
}

func (r *DistributedMatchRegistry) NewMatch(logger *zap.Logger, id uuid.UUID, core RuntimeMatchCore, stopped *atomic.Bool, params map[string]interface{}) (*MatchHandler, error) {
	return r.localRegistry.NewMatch(logger, id, core, stopped, params)
}

func (r *DistributedMatchRegistry) GetMatch(ctx context.Context, id string) (*api.Match, string, error) {
	_, node := r.parseMatchNode(id)
	if node == "" || node == r.node {
		return r.localRegistry.GetMatch(ctx, id)
	}

	// Remote match - request from owning node
	resp, err := r.cluster.Request(node, ClusterMessageType_MatchRPC, []byte(fmt.Sprintf(`{"op":"get","id":"%s"}`, id)))
	if err != nil {
		return nil, "", fmt.Errorf("failed to get remote match: %w", err)
	}

	var match api.Match
	if err := json.Unmarshal(resp, &match); err != nil {
		return nil, "", fmt.Errorf("failed to decode remote match: %w", err)
	}
	return &match, "", nil
}

func (r *DistributedMatchRegistry) RemoveMatch(id uuid.UUID, stream PresenceStream) {
	r.localRegistry.RemoveMatch(id, stream)
}

func (r *DistributedMatchRegistry) UpdateMatchLabel(id uuid.UUID, tickRate int, handlerName, label string, createTime int64) error {
	return r.localRegistry.UpdateMatchLabel(id, tickRate, handlerName, label, createTime)
}

func (r *DistributedMatchRegistry) ClearMatchLabel(id uuid.UUID, node string) error {
	return r.localRegistry.ClearMatchLabel(id, node)
}

func (r *DistributedMatchRegistry) ListMatches(ctx context.Context, limit int, authoritative *wrapperspb.BoolValue, label *wrapperspb.StringValue, minSize *wrapperspb.Int32Value, maxSize *wrapperspb.Int32Value, query *wrapperspb.StringValue, node *wrapperspb.StringValue) ([]*api.Match, []string, error) {
	return r.localRegistry.ListMatches(ctx, limit, authoritative, label, minSize, maxSize, query, node)
}

func (r *DistributedMatchRegistry) Stop(graceSeconds int) chan struct{} {
	return r.localRegistry.Stop(graceSeconds)
}

func (r *DistributedMatchRegistry) Count() int {
	return r.localRegistry.Count() + r.RemoteCount()
}

func (r *DistributedMatchRegistry) RemoteCount() int {
	r.remoteIndexMu.RLock()
	defer r.remoteIndexMu.RUnlock()
	count := 0
	for _, entries := range r.remoteIndex {
		count += len(entries)
	}
	return count
}

func (r *DistributedMatchRegistry) JoinAttempt(ctx context.Context, id uuid.UUID, node string, userID, sessionID uuid.UUID, username string, sessionExpiry int64, vars map[string]string, clientIP, clientPort, fromNode string, metadata map[string]string) (bool, bool, bool, string, string, []*MatchPresence) {
	if node == "" || node == r.node {
		return r.localRegistry.JoinAttempt(ctx, id, node, userID, sessionID, username, sessionExpiry, vars, clientIP, clientPort, fromNode, metadata)
	}
	// Remote - would need cluster.Request, for now delegate to local
	return r.localRegistry.JoinAttempt(ctx, id, node, userID, sessionID, username, sessionExpiry, vars, clientIP, clientPort, fromNode, metadata)
}

func (r *DistributedMatchRegistry) Join(id uuid.UUID, presences []*MatchPresence) {
	r.localRegistry.Join(id, presences)
}

func (r *DistributedMatchRegistry) Leave(id uuid.UUID, presences []*MatchPresence) {
	r.localRegistry.Leave(id, presences)
}

func (r *DistributedMatchRegistry) Kick(stream PresenceStream, presences []*MatchPresence) {
	r.localRegistry.Kick(stream, presences)
}

func (r *DistributedMatchRegistry) SendData(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string, opCode int64, data []byte, reliable bool, receiveTime int64) {
	if node == "" || node == r.node {
		r.localRegistry.SendData(id, node, userID, sessionID, username, fromNode, opCode, data, reliable, receiveTime)
		return
	}
	// Remote - forward via cluster
	r.localRegistry.SendData(id, node, userID, sessionID, username, fromNode, opCode, data, reliable, receiveTime)
}

func (r *DistributedMatchRegistry) Signal(ctx context.Context, id, data string) (string, error) {
	_, node := r.parseMatchNode(id)
	if node == "" || node == r.node {
		return r.localRegistry.Signal(ctx, id, data)
	}
	return r.localRegistry.Signal(ctx, id, data)
}

func (r *DistributedMatchRegistry) GetState(ctx context.Context, id uuid.UUID, node string) ([]*rtapi.UserPresence, int64, string, error) {
	if node == "" || node == r.node {
		return r.localRegistry.GetState(ctx, id, node)
	}
	return r.localRegistry.GetState(ctx, id, node)
}

func (r *DistributedMatchRegistry) handleIndexSync(msg *ClusterMessage) {
	var entries []*distributedMatchIndexEntry
	if err := json.Unmarshal(msg.Payload, &entries); err != nil {
		r.logger.Warn("Failed to decode match index sync", zap.Error(err))
		return
	}

	r.remoteIndexMu.Lock()
	r.remoteIndex[msg.SourceNode] = entries
	r.remoteIndexMu.Unlock()
}

func (r *DistributedMatchRegistry) handleMatchRPC(msg *ClusterMessage) {
	var rpc struct {
		Op string `json:"op"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(msg.Payload, &rpc); err != nil {
		r.logger.Warn("Failed to decode match RPC", zap.Error(err))
		return
	}

	switch rpc.Op {
	case "get":
		match, _, err := r.localRegistry.GetMatch(context.Background(), rpc.ID)
		if err != nil || match == nil {
			r.cluster.Reply(msg, []byte("null"))
			return
		}
		data, _ := json.Marshal(match)
		r.cluster.Reply(msg, data)
	}
}

func (r *DistributedMatchRegistry) handleNodeLeave(nodeName string) {
	r.remoteIndexMu.Lock()
	delete(r.remoteIndex, nodeName)
	r.remoteIndexMu.Unlock()
}
