package server

import (
	"context"
	"encoding/json"

	"github.com/heroiclabs/nakama/v3/console"
	"go.uber.org/zap"
)

type DistributedStatusHandler struct {
	localHandler StatusHandler
	cluster      ClusterClient
	node         string
	logger       *zap.Logger
}

func NewDistributedStatusHandler(logger *zap.Logger, localHandler StatusHandler, cluster ClusterClient, node string) *DistributedStatusHandler {
	dsh := &DistributedStatusHandler{
		localHandler: localHandler,
		cluster:      cluster,
		node:         node,
		logger:       logger,
	}

	cluster.RegisterHandler(ClusterMessageType_StatusRequest, dsh.handleStatusRequest)

	return dsh
}

func (h *DistributedStatusHandler) GetStatus(ctx context.Context) ([]*console.StatusList_Status, error) {
	// Get local status
	localStatuses, err := h.localHandler.GetStatus(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*console.StatusList_Status, 0, len(localStatuses))
	result = append(result, localStatuses...)

	// Query all remote nodes
	for _, memberName := range h.cluster.MemberNames() {
		if memberName == h.node {
			continue
		}

		resp, err := h.cluster.Request(memberName, ClusterMessageType_StatusRequest, nil)
		if err != nil {
			h.logger.Warn("Failed to get status from remote node", zap.String("node", memberName), zap.Error(err))
			continue
		}

		var remoteStatuses []*console.StatusList_Status
		if err := json.Unmarshal(resp, &remoteStatuses); err != nil {
			h.logger.Warn("Failed to decode remote status", zap.String("node", memberName), zap.Error(err))
			continue
		}

		result = append(result, remoteStatuses...)
	}

	return result, nil
}

func (h *DistributedStatusHandler) handleStatusRequest(msg *ClusterMessage) {
	statuses, err := h.localHandler.GetStatus(context.Background())
	if err != nil {
		h.logger.Warn("Failed to get local status for remote request", zap.Error(err))
		h.cluster.Reply(msg, []byte("[]"))
		return
	}

	data, _ := json.Marshal(statuses)
	h.cluster.Reply(msg, data)
}
