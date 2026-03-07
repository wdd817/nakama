package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"go.uber.org/zap"
)

type distributedPartyIndexEntry struct {
	PartyID string `json:"party_id"`
	Node    string `json:"node"`
	Open    bool   `json:"open,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
	MaxSize int    `json:"max_size,omitempty"`
	Label   string `json:"label,omitempty"`
}

type DistributedPartyRegistry struct {
	localRegistry PartyRegistry
	remoteIndex   map[string][]*distributedPartyIndexEntry // node -> entries
	remoteIndexMu sync.RWMutex
	cluster       ClusterClient
	node          string
	logger        *zap.Logger
}

func NewDistributedPartyRegistry(logger *zap.Logger, localRegistry PartyRegistry, cluster ClusterClient, node string) *DistributedPartyRegistry {
	dp := &DistributedPartyRegistry{
		localRegistry: localRegistry,
		remoteIndex:   make(map[string][]*distributedPartyIndexEntry),
		cluster:       cluster,
		node:          node,
		logger:        logger,
	}

	cluster.RegisterHandler(ClusterMessageType_PartyIndexSync, dp.handleIndexSync)
	cluster.RegisterHandler(ClusterMessageType_PartyRPC, dp.handlePartyRPC)
	cluster.OnNodeLeave(dp.handleNodeLeave)

	return dp
}

func (r *DistributedPartyRegistry) Init(matchmaker Matchmaker, tracker Tracker, streamManager StreamManager, router MessageRouter) {
	r.localRegistry.Init(matchmaker, tracker, streamManager, router)
}

func (r *DistributedPartyRegistry) Create(open, hidden bool, maxSize int, leader *rtapi.UserPresence, label string) (*PartyHandler, error) {
	handler, err := r.localRegistry.Create(open, hidden, maxSize, leader, label)
	if err != nil {
		return nil, err
	}

	entry := &distributedPartyIndexEntry{
		Node:    r.node,
		Open:    open,
		Hidden:  hidden,
		MaxSize: maxSize,
		Label:   label,
	}
	payload, _ := json.Marshal([]*distributedPartyIndexEntry{entry})
	r.cluster.Broadcast(ClusterMessageType_PartyIndexSync, payload)

	return handler, nil
}

func (r *DistributedPartyRegistry) Delete(id uuid.UUID) {
	r.localRegistry.Delete(id)
}

func (r *DistributedPartyRegistry) Count() int {
	return r.localRegistry.Count() + r.RemoteCount()
}

func (r *DistributedPartyRegistry) RemoteCount() int {
	r.remoteIndexMu.RLock()
	defer r.remoteIndexMu.RUnlock()
	count := 0
	for _, entries := range r.remoteIndex {
		count += len(entries)
	}
	return count
}

func (r *DistributedPartyRegistry) Join(id uuid.UUID, presences []*Presence) {
	r.localRegistry.Join(id, presences)
}

func (r *DistributedPartyRegistry) Leave(id uuid.UUID, presences []*Presence) {
	r.localRegistry.Leave(id, presences)
}

func (r *DistributedPartyRegistry) PartyJoinRequest(ctx context.Context, id uuid.UUID, node string, presence *Presence) (bool, error) {
	return r.localRegistry.PartyJoinRequest(ctx, id, node, presence)
}

func (r *DistributedPartyRegistry) PartyPromote(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return r.localRegistry.PartyPromote(ctx, id, node, sessionID, fromNode, presence)
}

func (r *DistributedPartyRegistry) PartyAccept(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return r.localRegistry.PartyAccept(ctx, id, node, sessionID, fromNode, presence)
}

func (r *DistributedPartyRegistry) PartyRemove(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return r.localRegistry.PartyRemove(ctx, id, node, sessionID, fromNode, presence)
}

func (r *DistributedPartyRegistry) PartyClose(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string) error {
	return r.localRegistry.PartyClose(ctx, id, node, sessionID, fromNode)
}

func (r *DistributedPartyRegistry) PartyJoinRequestList(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string) ([]*rtapi.UserPresence, error) {
	return r.localRegistry.PartyJoinRequestList(ctx, id, node, sessionID, fromNode)
}

func (r *DistributedPartyRegistry) PartyMatchmakerAdd(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) (string, []*PresenceID, error) {
	return r.localRegistry.PartyMatchmakerAdd(ctx, id, node, sessionID, fromNode, query, minCount, maxCount, countMultiple, stringProperties, numericProperties)
}

func (r *DistributedPartyRegistry) PartyMatchmakerRemove(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, ticket string) error {
	return r.localRegistry.PartyMatchmakerRemove(ctx, id, node, sessionID, fromNode, ticket)
}

func (r *DistributedPartyRegistry) PartyDataSend(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, opCode int64, data []byte) error {
	return r.localRegistry.PartyDataSend(ctx, id, node, sessionID, fromNode, opCode, data)
}

func (r *DistributedPartyRegistry) PartyUpdate(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, label string, open, hidden bool) error {
	return r.localRegistry.PartyUpdate(ctx, id, node, sessionID, fromNode, label, open, hidden)
}

func (r *DistributedPartyRegistry) PartyList(ctx context.Context, limit int, open *bool, showHidden bool, query, cursor string) ([]*api.Party, string, error) {
	return r.localRegistry.PartyList(ctx, limit, open, showHidden, query, cursor)
}

func (r *DistributedPartyRegistry) LabelUpdate(id uuid.UUID, node, label string, open, hidden bool, maxSize int, createTime time.Time) error {
	return r.localRegistry.LabelUpdate(id, node, label, open, hidden, maxSize, createTime)
}

func (r *DistributedPartyRegistry) handleIndexSync(msg *ClusterMessage) {
	var entries []*distributedPartyIndexEntry
	if err := json.Unmarshal(msg.Payload, &entries); err != nil {
		r.logger.Warn("Failed to decode party index sync", zap.Error(err))
		return
	}

	r.remoteIndexMu.Lock()
	r.remoteIndex[msg.SourceNode] = entries
	r.remoteIndexMu.Unlock()
}

func (r *DistributedPartyRegistry) handlePartyRPC(msg *ClusterMessage) {
	// Placeholder for future cross-node party RPC handling
}

func (r *DistributedPartyRegistry) handleNodeLeave(nodeName string) {
	r.remoteIndexMu.Lock()
	delete(r.remoteIndex, nodeName)
	r.remoteIndexMu.Unlock()
}
