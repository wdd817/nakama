package server

import (
	"encoding/json"
	"sync"

	"github.com/gofrs/uuid/v5"
)

// distributedPresenceEvent is the wire format for presence events broadcast across the cluster.
type distributedPresenceEvent struct {
	Join       bool           `json:"join"`
	UntrackAll bool           `json:"untrack_all,omitempty"`
	Node       string         `json:"node"`
	SessionID  uuid.UUID      `json:"session_id"`
	UserID     uuid.UUID      `json:"user_id"`
	Stream     PresenceStream `json:"stream"`
	Meta       PresenceMeta   `json:"meta"`
}

type presenceKey struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
}

type peerState struct {
	presences map[PresenceStream]map[presenceKey]*Presence
}

// DistributedTracker manages remote presence state received from other cluster nodes.
// It does NOT implement the full Tracker interface — it is a companion to LocalTracker.
// The assembly layer (main.go) is responsible for merging local and remote results.
type DistributedTracker struct {
	cluster     ClusterClient
	node        string
	remotePeers map[string]*peerState // node_name -> remote presences
	remoteMu    sync.RWMutex
}

func NewDistributedTracker(cluster ClusterClient, node string) *DistributedTracker {
	dt := &DistributedTracker{
		cluster:     cluster,
		node:        node,
		remotePeers: make(map[string]*peerState),
	}

	cluster.RegisterHandler(ClusterMessageType_PresenceEvent, dt.handlePresenceEvent)
	cluster.OnNodeLeave(dt.handleNodeLeave)

	return dt
}

// TrackRemotePresence broadcasts a presence join event to the cluster.
func (dt *DistributedTracker) TrackRemotePresence(sessionID uuid.UUID, stream PresenceStream, userID uuid.UUID, meta PresenceMeta, node string) {
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      node,
		SessionID: sessionID,
		UserID:    userID,
		Stream:    stream,
		Meta:      meta,
	}
	payload, _ := json.Marshal(event)
	dt.cluster.Broadcast(ClusterMessageType_PresenceEvent, payload)
}

// UntrackRemotePresence broadcasts a presence leave event to the cluster.
func (dt *DistributedTracker) UntrackRemotePresence(sessionID uuid.UUID, stream PresenceStream, userID uuid.UUID, node string) {
	event := &distributedPresenceEvent{
		Join:      false,
		Node:      node,
		SessionID: sessionID,
		UserID:    userID,
		Stream:    stream,
	}
	payload, _ := json.Marshal(event)
	dt.cluster.Broadcast(ClusterMessageType_PresenceEvent, payload)
}

// UntrackAllRemotePresences broadcasts an untrack-all event for a session.
func (dt *DistributedTracker) UntrackAllRemotePresences(sessionID uuid.UUID, node string) {
	event := &distributedPresenceEvent{
		Join:       false,
		UntrackAll: true,
		Node:       node,
		SessionID:  sessionID,
	}
	payload, _ := json.Marshal(event)
	dt.cluster.Broadcast(ClusterMessageType_PresenceEvent, payload)
}

// ListByStream returns remote presences for a given stream, optionally filtering by hidden/not-hidden.
func (dt *DistributedTracker) ListByStream(stream PresenceStream, includeHidden bool, includeNotHidden bool) []*Presence {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	var result []*Presence
	for _, peer := range dt.remotePeers {
		if streamPresences, ok := peer.presences[stream]; ok {
			for _, p := range streamPresences {
				if p.Meta.Hidden && !includeHidden {
					continue
				}
				if !p.Meta.Hidden && !includeNotHidden {
					continue
				}
				result = append(result, p)
			}
		}
	}
	return result
}

// ListLocalByStream returns nothing — remote presences are never local.
func (dt *DistributedTracker) ListLocalByStream(stream PresenceStream) []*Presence {
	return nil
}

// CountByStream returns the count of remote presences for a given stream.
func (dt *DistributedTracker) CountByStream(stream PresenceStream) int {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	count := 0
	for _, peer := range dt.remotePeers {
		if streamPresences, ok := peer.presences[stream]; ok {
			count += len(streamPresences)
		}
	}
	return count
}

// Count returns the total count of all remote presences.
func (dt *DistributedTracker) Count() int {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	count := 0
	for _, peer := range dt.remotePeers {
		for _, streamPresences := range peer.presences {
			count += len(streamPresences)
		}
	}
	return count
}

// ListPresenceIDByStream returns remote presence IDs for a given stream.
func (dt *DistributedTracker) ListPresenceIDByStream(stream PresenceStream) []*PresenceID {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	var result []*PresenceID
	for _, peer := range dt.remotePeers {
		if streamPresences, ok := peer.presences[stream]; ok {
			for _, p := range streamPresences {
				pid := p.ID
				result = append(result, &pid)
			}
		}
	}
	return result
}

// ListNodesForStream returns the set of remote nodes that have presences for the given stream.
func (dt *DistributedTracker) ListNodesForStream(stream PresenceStream) map[string]struct{} {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	nodes := make(map[string]struct{})
	for nodeName, peer := range dt.remotePeers {
		if streamPresences, ok := peer.presences[stream]; ok && len(streamPresences) > 0 {
			nodes[nodeName] = struct{}{}
		}
	}
	return nodes
}

// StreamExists returns true if any remote node has presences for the given stream.
func (dt *DistributedTracker) StreamExists(stream PresenceStream) bool {
	dt.remoteMu.RLock()
	defer dt.remoteMu.RUnlock()

	for _, peer := range dt.remotePeers {
		if streamPresences, ok := peer.presences[stream]; ok && len(streamPresences) > 0 {
			return true
		}
	}
	return false
}

func (dt *DistributedTracker) handlePresenceEvent(msg *ClusterMessage) {
	var event distributedPresenceEvent
	if err := json.Unmarshal(msg.Payload, &event); err != nil {
		return
	}

	dt.remoteMu.Lock()
	defer dt.remoteMu.Unlock()

	if event.UntrackAll {
		// Remove all presences for this session from the source node
		if peer, ok := dt.remotePeers[event.Node]; ok {
			for stream, streamPresences := range peer.presences {
				for key := range streamPresences {
					if key.SessionID == event.SessionID {
						delete(streamPresences, key)
					}
				}
				if len(streamPresences) == 0 {
					delete(peer.presences, stream)
				}
			}
			if len(peer.presences) == 0 {
				delete(dt.remotePeers, event.Node)
			}
		}
		return
	}

	if event.Join {
		peer, ok := dt.remotePeers[event.Node]
		if !ok {
			peer = &peerState{presences: make(map[PresenceStream]map[presenceKey]*Presence)}
			dt.remotePeers[event.Node] = peer
		}
		streamPresences, ok := peer.presences[event.Stream]
		if !ok {
			streamPresences = make(map[presenceKey]*Presence)
			peer.presences[event.Stream] = streamPresences
		}
		key := presenceKey{SessionID: event.SessionID, UserID: event.UserID}
		streamPresences[key] = &Presence{
			ID:     PresenceID{Node: event.Node, SessionID: event.SessionID},
			Stream: event.Stream,
			UserID: event.UserID,
			Meta:   event.Meta,
		}
	} else {
		if peer, ok := dt.remotePeers[event.Node]; ok {
			if streamPresences, ok := peer.presences[event.Stream]; ok {
				key := presenceKey{SessionID: event.SessionID, UserID: event.UserID}
				delete(streamPresences, key)
				if len(streamPresences) == 0 {
					delete(peer.presences, event.Stream)
				}
			}
			if len(peer.presences) == 0 {
				delete(dt.remotePeers, event.Node)
			}
		}
	}
}

func (dt *DistributedTracker) handleNodeLeave(nodeName string) {
	dt.remoteMu.Lock()
	delete(dt.remotePeers, nodeName)
	dt.remoteMu.Unlock()
}
