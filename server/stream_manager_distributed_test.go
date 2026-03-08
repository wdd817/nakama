package server

import (
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockStreamManager implements StreamManager for testing.
type mockStreamManager struct {
	joinCalled   int
	updateCalled int
	leaveCalled  int
}

func (m *mockStreamManager) UserJoin(stream PresenceStream, userID, sessionID uuid.UUID, hidden, persistence bool, status string) (bool, bool, error) {
	m.joinCalled++
	return true, true, nil
}

func (m *mockStreamManager) UserUpdate(stream PresenceStream, userID, sessionID uuid.UUID, hidden, persistence bool, status string) (bool, error) {
	m.updateCalled++
	return true, nil
}

func (m *mockStreamManager) UserLeave(stream PresenceStream, userID, sessionID uuid.UUID) error {
	m.leaveCalled++
	return nil
}

func TestDistributedStreamManagerLocalJoin(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockStreamManager{}
	logger := zap.NewNop()

	dsm := NewDistributedStreamManager(logger, local, cluster, "node1")

	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	userID := uuid.Must(uuid.NewV4())
	sessionID := uuid.Must(uuid.NewV4())

	_, _, err := dsm.UserJoin(stream, userID, sessionID, false, true, "online")
	require.NoError(t, err)
	assert.Equal(t, 1, local.joinCalled)
}

func TestDistributedStreamManagerLocalUpdate(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockStreamManager{}
	logger := zap.NewNop()

	dsm := NewDistributedStreamManager(logger, local, cluster, "node1")

	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	userID := uuid.Must(uuid.NewV4())
	sessionID := uuid.Must(uuid.NewV4())

	_, err := dsm.UserUpdate(stream, userID, sessionID, false, true, "away")
	require.NoError(t, err)
	assert.Equal(t, 1, local.updateCalled)
}

func TestDistributedStreamManagerLocalLeave(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockStreamManager{}
	logger := zap.NewNop()

	dsm := NewDistributedStreamManager(logger, local, cluster, "node1")

	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	userID := uuid.Must(uuid.NewV4())
	sessionID := uuid.Must(uuid.NewV4())

	err := dsm.UserLeave(stream, userID, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 1, local.leaveCalled)
}
