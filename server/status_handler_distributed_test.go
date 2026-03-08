package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/heroiclabs/nakama/v3/console"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockStatusHandler implements StatusHandler for testing.
type mockStatusHandler struct {
	status []*console.StatusList_Status
}

func (m *mockStatusHandler) GetStatus(ctx context.Context) ([]*console.StatusList_Status, error) {
	return m.status, nil
}

func TestDistributedStatusHandlerLocal(t *testing.T) {
	cluster := newMockClusterClient("node1")
	local := &mockStatusHandler{
		status: []*console.StatusList_Status{
			{Name: "node1", SessionCount: 10},
		},
	}
	logger := zap.NewNop()

	dsh := NewDistributedStatusHandler(logger, local, cluster, "node1")

	statuses, err := dsh.GetStatus(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "node1", statuses[0].Name)
	assert.Equal(t, int32(10), statuses[0].SessionCount)
}

func TestDistributedStatusHandlerMergesRemote(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockStatusHandler{
		status: []*console.StatusList_Status{
			{Name: "node1", SessionCount: 10},
		},
	}
	logger := zap.NewNop()

	// Set up cluster to respond with remote status
	cluster.requestHandler = func(nodeName string, msgType ClusterMessageType, payload []byte) ([]byte, error) {
		remoteStatus := []*console.StatusList_Status{
			{Name: "node2", SessionCount: 20},
		}
		data, _ := json.Marshal(remoteStatus)
		return data, nil
	}

	dsh := NewDistributedStatusHandler(logger, local, cluster, "node1")

	statuses, err := dsh.GetStatus(context.Background())
	require.NoError(t, err)
	require.Len(t, statuses, 2)

	// Find node2 status
	var node2Found bool
	for _, s := range statuses {
		if s.Name == "node2" {
			assert.Equal(t, int32(20), s.SessionCount)
			node2Found = true
		}
	}
	assert.True(t, node2Found, "node2 status should be present")
}
