package server

import (
	"github.com/gofrs/uuid/v5"
	"go.uber.org/zap"
)

type DistributedStreamManager struct {
	localStreamManager StreamManager
	cluster            ClusterClient
	node               string
	logger             *zap.Logger
}

func NewDistributedStreamManager(logger *zap.Logger, localStreamManager StreamManager, cluster ClusterClient, node string) *DistributedStreamManager {
	dsm := &DistributedStreamManager{
		localStreamManager: localStreamManager,
		cluster:            cluster,
		node:               node,
		logger:             logger,
	}

	cluster.RegisterHandler(ClusterMessageType_StreamRPC, dsm.handleStreamRPC)

	return dsm
}

func (m *DistributedStreamManager) UserJoin(stream PresenceStream, userID, sessionID uuid.UUID, hidden, persistence bool, status string) (bool, bool, error) {
	return m.localStreamManager.UserJoin(stream, userID, sessionID, hidden, persistence, status)
}

func (m *DistributedStreamManager) UserUpdate(stream PresenceStream, userID, sessionID uuid.UUID, hidden, persistence bool, status string) (bool, error) {
	return m.localStreamManager.UserUpdate(stream, userID, sessionID, hidden, persistence, status)
}

func (m *DistributedStreamManager) UserLeave(stream PresenceStream, userID, sessionID uuid.UUID) error {
	return m.localStreamManager.UserLeave(stream, userID, sessionID)
}

func (m *DistributedStreamManager) handleStreamRPC(msg *ClusterMessage) {
	// Placeholder for future cross-node stream RPC handling
}
