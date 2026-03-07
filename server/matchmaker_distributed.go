package server

import (
	"context"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/api"
	"go.uber.org/zap"
)

type distributedMatchmakerRemoval struct {
	SessionID string   `json:"session_id,omitempty"`
	PartyID   string   `json:"party_id,omitempty"`
	Ticket    string   `json:"ticket,omitempty"`
	Tickets   []string `json:"tickets,omitempty"`
	All       bool     `json:"all,omitempty"`
}

type DistributedMatchmaker struct {
	localMatchmaker Matchmaker
	cluster         ClusterClient
	node            string
	logger          *zap.Logger
	ctx             context.Context
	ctxCancelFn     context.CancelFunc
}

func NewDistributedMatchmaker(logger *zap.Logger, localMatchmaker Matchmaker, cluster ClusterClient, node string, syncInterval interface{}) *DistributedMatchmaker {
	ctx, cancel := context.WithCancel(context.Background())
	dm := &DistributedMatchmaker{
		localMatchmaker: localMatchmaker,
		cluster:         cluster,
		node:            node,
		logger:          logger,
		ctx:             ctx,
		ctxCancelFn:     cancel,
	}

	cluster.RegisterHandler(ClusterMessageType_MatchmakerSync, dm.handleSync)
	cluster.RegisterHandler(ClusterMessageType_MatchmakerRemove, dm.handleRemove)
	cluster.OnNodeLeave(dm.handleNodeLeave)

	return dm
}

func (dm *DistributedMatchmaker) Stop() {
	dm.ctxCancelFn()
	dm.localMatchmaker.Stop()
}

func (dm *DistributedMatchmaker) Pause() {
	dm.localMatchmaker.Pause()
}

func (dm *DistributedMatchmaker) Resume() {
	dm.localMatchmaker.Resume()
}

func (dm *DistributedMatchmaker) OnMatchedEntries(fn func(entries [][]*MatchmakerEntry)) {
	dm.localMatchmaker.OnMatchedEntries(fn)
}

func (dm *DistributedMatchmaker) OnStatsUpdate(fn func(stats *api.MatchmakerStats)) {
	dm.localMatchmaker.OnStatsUpdate(fn)
}

func (dm *DistributedMatchmaker) Add(ctx context.Context, presences []*MatchmakerPresence, sessionID, partyId, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) (string, int64, error) {
	ticket, createTime, err := dm.localMatchmaker.Add(ctx, presences, sessionID, partyId, query, minCount, maxCount, countMultiple, stringProperties, numericProperties)
	if err != nil {
		return "", 0, err
	}

	// Broadcast the new ticket to other nodes
	extract := &MatchmakerExtract{
		Presences:         presences,
		SessionID:         sessionID,
		PartyId:           partyId,
		Query:             query,
		MinCount:          minCount,
		MaxCount:          maxCount,
		CountMultiple:     countMultiple,
		StringProperties:  stringProperties,
		NumericProperties: numericProperties,
		Ticket:            ticket,
		Node:              dm.node,
		CreatedAt:         createTime,
	}
	payload, _ := json.Marshal([]*MatchmakerExtract{extract})
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerSync, payload)

	return ticket, createTime, nil
}

func (dm *DistributedMatchmaker) Insert(extracts []*MatchmakerExtract) error {
	return dm.localMatchmaker.Insert(extracts)
}

func (dm *DistributedMatchmaker) Extract() []*MatchmakerExtract {
	return dm.localMatchmaker.Extract()
}

func (dm *DistributedMatchmaker) RemoveSession(sessionID, ticket string) error {
	err := dm.localMatchmaker.RemoveSession(sessionID, ticket)
	if err != nil {
		return err
	}
	removal := &distributedMatchmakerRemoval{SessionID: sessionID, Ticket: ticket}
	payload, _ := json.Marshal(removal)
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerRemove, payload)
	return nil
}

func (dm *DistributedMatchmaker) RemoveSessionAll(sessionID string) error {
	err := dm.localMatchmaker.RemoveSessionAll(sessionID)
	if err != nil {
		return err
	}
	removal := &distributedMatchmakerRemoval{SessionID: sessionID, All: true}
	payload, _ := json.Marshal(removal)
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerRemove, payload)
	return nil
}

func (dm *DistributedMatchmaker) RemoveParty(partyID, ticket string) error {
	err := dm.localMatchmaker.RemoveParty(partyID, ticket)
	if err != nil {
		return err
	}
	removal := &distributedMatchmakerRemoval{PartyID: partyID, Ticket: ticket}
	payload, _ := json.Marshal(removal)
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerRemove, payload)
	return nil
}

func (dm *DistributedMatchmaker) RemovePartyAll(partyID string) error {
	err := dm.localMatchmaker.RemovePartyAll(partyID)
	if err != nil {
		return err
	}
	removal := &distributedMatchmakerRemoval{PartyID: partyID, All: true}
	payload, _ := json.Marshal(removal)
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerRemove, payload)
	return nil
}

func (dm *DistributedMatchmaker) RemoveAll(node string) {
	dm.localMatchmaker.RemoveAll(node)
}

func (dm *DistributedMatchmaker) Remove(tickets []string) {
	dm.localMatchmaker.Remove(tickets)
	removal := &distributedMatchmakerRemoval{Tickets: tickets}
	payload, _ := json.Marshal(removal)
	dm.cluster.Broadcast(ClusterMessageType_MatchmakerRemove, payload)
}

func (dm *DistributedMatchmaker) GetStats() *api.MatchmakerStats {
	return dm.localMatchmaker.GetStats()
}

func (dm *DistributedMatchmaker) SetStats(stats *api.MatchmakerStats) {
	dm.localMatchmaker.SetStats(stats)
}

func (dm *DistributedMatchmaker) handleSync(msg *ClusterMessage) {
	var extracts []*MatchmakerExtract
	if err := json.Unmarshal(msg.Payload, &extracts); err != nil {
		dm.logger.Warn("Failed to decode matchmaker sync", zap.Error(err))
		return
	}
	if err := dm.localMatchmaker.Insert(extracts); err != nil {
		dm.logger.Warn("Failed to insert remote matchmaker tickets", zap.Error(err))
	}
}

func (dm *DistributedMatchmaker) handleRemove(msg *ClusterMessage) {
	var removal distributedMatchmakerRemoval
	if err := json.Unmarshal(msg.Payload, &removal); err != nil {
		dm.logger.Warn("Failed to decode matchmaker removal", zap.Error(err))
		return
	}

	if len(removal.Tickets) > 0 {
		dm.localMatchmaker.Remove(removal.Tickets)
		return
	}

	if removal.SessionID != "" {
		if removal.All {
			dm.localMatchmaker.RemoveSessionAll(removal.SessionID)
		} else if removal.Ticket != "" {
			dm.localMatchmaker.RemoveSession(removal.SessionID, removal.Ticket)
		}
		return
	}

	if removal.PartyID != "" {
		if removal.All {
			dm.localMatchmaker.RemovePartyAll(removal.PartyID)
		} else if removal.Ticket != "" {
			dm.localMatchmaker.RemoveParty(removal.PartyID, removal.Ticket)
		}
	}
}

func (dm *DistributedMatchmaker) handleNodeLeave(nodeName string) {
	dm.localMatchmaker.RemoveAll(nodeName)
}
