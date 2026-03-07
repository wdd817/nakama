package server

import (
	"encoding/json"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/rtapi"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type envelopePayload struct {
	PresenceIDs []presenceIDWire `json:"p"`
	Envelope    []byte           `json:"e"`
	Reliable    bool             `json:"r"`
}

type presenceIDWire struct {
	Node      string    `json:"n"`
	SessionID uuid.UUID `json:"s"`
}

func encodeEnvelopePayload(presenceIDs []*PresenceID, envBytes []byte, reliable bool) []byte {
	wires := make([]presenceIDWire, len(presenceIDs))
	for i, p := range presenceIDs {
		wires[i] = presenceIDWire{Node: p.Node, SessionID: p.SessionID}
	}
	payload, _ := json.Marshal(&envelopePayload{
		PresenceIDs: wires,
		Envelope:    envBytes,
		Reliable:    reliable,
	})
	return payload
}

func decodeEnvelopePayload(data []byte) ([]*PresenceID, []byte, bool, error) {
	var ep envelopePayload
	if err := json.Unmarshal(data, &ep); err != nil {
		return nil, nil, false, err
	}
	presenceIDs := make([]*PresenceID, len(ep.PresenceIDs))
	for i, w := range ep.PresenceIDs {
		presenceIDs[i] = &PresenceID{Node: w.Node, SessionID: w.SessionID}
	}
	return presenceIDs, ep.Envelope, ep.Reliable, nil
}

type DistributedMessageRouter struct {
	localRouter MessageRouter
	cluster     ClusterClient
	node        string
	logger      *zap.Logger
}

func NewDistributedMessageRouter(logger *zap.Logger, localRouter MessageRouter, cluster ClusterClient, node string) *DistributedMessageRouter {
	dr := &DistributedMessageRouter{
		localRouter: localRouter,
		cluster:     cluster,
		node:        node,
		logger:      logger,
	}

	cluster.RegisterHandler(ClusterMessageType_Envelope, dr.handleEnvelope)
	cluster.RegisterHandler(ClusterMessageType_EnvelopeBroadcast, dr.handleEnvelopeBroadcast)

	return dr
}

func (r *DistributedMessageRouter) SendToPresenceIDs(logger *zap.Logger, presenceIDs []*PresenceID, envelope *rtapi.Envelope, reliable bool) {
	// Partition by node
	local := make([]*PresenceID, 0, len(presenceIDs))
	remote := make(map[string][]*PresenceID) // node -> presenceIDs

	for _, pid := range presenceIDs {
		if pid.Node == r.node {
			local = append(local, pid)
		} else {
			remote[pid.Node] = append(remote[pid.Node], pid)
		}
	}

	// Deliver local
	if len(local) > 0 {
		r.localRouter.SendToPresenceIDs(logger, local, envelope, reliable)
	}

	// Forward remote
	if len(remote) > 0 {
		envBytes, err := proto.Marshal(envelope)
		if err != nil {
			logger.Warn("Failed to marshal envelope for remote delivery", zap.Error(err))
			return
		}

		for nodeName, pids := range remote {
			payload := encodeEnvelopePayload(pids, envBytes, reliable)
			if err := r.cluster.SendTo(nodeName, ClusterMessageType_Envelope, payload); err != nil {
				logger.Warn("Failed to forward envelope to remote node", zap.String("node", nodeName), zap.Error(err))
			}
		}
	}
}

func (r *DistributedMessageRouter) SendToStream(logger *zap.Logger, stream PresenceStream, envelope *rtapi.Envelope, reliable bool) {
	// Delegate to local router — it uses the tracker which already includes remote presences
	r.localRouter.SendToStream(logger, stream, envelope, reliable)
}

func (r *DistributedMessageRouter) SendDeferred(logger *zap.Logger, messages []*DeferredMessage) {
	for _, msg := range messages {
		r.SendToPresenceIDs(logger, msg.PresenceIDs, msg.Envelope, msg.Reliable)
	}
}

func (r *DistributedMessageRouter) SendToAll(logger *zap.Logger, envelope *rtapi.Envelope, reliable bool) {
	// Deliver locally
	r.localRouter.SendToAll(logger, envelope, reliable)

	// Broadcast to all remote nodes
	envBytes, err := proto.Marshal(envelope)
	if err != nil {
		logger.Warn("Failed to marshal envelope for broadcast", zap.Error(err))
		return
	}

	payload, _ := json.Marshal(&envelopePayload{
		Envelope: envBytes,
		Reliable: reliable,
	})
	r.cluster.Broadcast(ClusterMessageType_EnvelopeBroadcast, payload)
}

func (r *DistributedMessageRouter) handleEnvelope(msg *ClusterMessage) {
	presenceIDs, envBytes, reliable, err := decodeEnvelopePayload(msg.Payload)
	if err != nil {
		r.logger.Warn("Failed to decode remote envelope payload", zap.Error(err))
		return
	}

	envelope := &rtapi.Envelope{}
	if err := proto.Unmarshal(envBytes, envelope); err != nil {
		r.logger.Warn("Failed to unmarshal remote envelope", zap.Error(err))
		return
	}

	r.localRouter.SendToPresenceIDs(r.logger, presenceIDs, envelope, reliable)
}

func (r *DistributedMessageRouter) handleEnvelopeBroadcast(msg *ClusterMessage) {
	var ep envelopePayload
	if err := json.Unmarshal(msg.Payload, &ep); err != nil {
		r.logger.Warn("Failed to decode remote broadcast payload", zap.Error(err))
		return
	}

	envelope := &rtapi.Envelope{}
	if err := proto.Unmarshal(ep.Envelope, envelope); err != nil {
		r.logger.Warn("Failed to unmarshal remote broadcast envelope", zap.Error(err))
		return
	}

	r.localRouter.SendToAll(r.logger, envelope, ep.Reliable)
}
