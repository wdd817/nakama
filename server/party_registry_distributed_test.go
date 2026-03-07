package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockPartyRegistry implements PartyRegistry for testing.
type mockPartyRegistry struct {
	initCalled    bool
	createCalled  int
	deleteCalled  []uuid.UUID
	count         int
	joinCalled    int
	leaveCalled   int
	labelUpdates  []string
	partyListResult []*api.Party
}

func (m *mockPartyRegistry) Init(matchmaker Matchmaker, tracker Tracker, streamManager StreamManager, router MessageRouter) {
	m.initCalled = true
}

func (m *mockPartyRegistry) Create(open, hidden bool, maxSize int, leader *rtapi.UserPresence, label string) (*PartyHandler, error) {
	m.createCalled++
	return nil, nil
}

func (m *mockPartyRegistry) Delete(id uuid.UUID) {
	m.deleteCalled = append(m.deleteCalled, id)
}

func (m *mockPartyRegistry) Count() int {
	return m.count
}

func (m *mockPartyRegistry) Join(id uuid.UUID, presences []*Presence) {
	m.joinCalled++
}

func (m *mockPartyRegistry) Leave(id uuid.UUID, presences []*Presence) {
	m.leaveCalled++
}

func (m *mockPartyRegistry) PartyJoinRequest(ctx context.Context, id uuid.UUID, node string, presence *Presence) (bool, error) {
	return true, nil
}

func (m *mockPartyRegistry) PartyPromote(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return nil
}

func (m *mockPartyRegistry) PartyAccept(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return nil
}

func (m *mockPartyRegistry) PartyRemove(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, presence *rtapi.UserPresence) error {
	return nil
}

func (m *mockPartyRegistry) PartyClose(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string) error {
	return nil
}

func (m *mockPartyRegistry) PartyJoinRequestList(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string) ([]*rtapi.UserPresence, error) {
	return nil, nil
}

func (m *mockPartyRegistry) PartyMatchmakerAdd(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) (string, []*PresenceID, error) {
	return "", nil, nil
}

func (m *mockPartyRegistry) PartyMatchmakerRemove(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, ticket string) error {
	return nil
}

func (m *mockPartyRegistry) PartyDataSend(ctx context.Context, id uuid.UUID, node, sessionID, fromNode string, opCode int64, data []byte) error {
	return nil
}

func (m *mockPartyRegistry) PartyUpdate(ctx context.Context, id uuid.UUID, node, sessionID, fromNode, label string, open, hidden bool) error {
	return nil
}

func (m *mockPartyRegistry) PartyList(ctx context.Context, limit int, open *bool, showHidden bool, query, cursor string) ([]*api.Party, string, error) {
	return m.partyListResult, "", nil
}

func (m *mockPartyRegistry) LabelUpdate(id uuid.UUID, node, label string, open, hidden bool, maxSize int, createTime time.Time) error {
	m.labelUpdates = append(m.labelUpdates, label)
	return nil
}

// --- Tests ---

func TestDistributedPartyRegistryCreateBroadcastsIndex(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockPartyRegistry{}
	logger := zap.NewNop()

	dp := NewDistributedPartyRegistry(logger, local, cluster, "node1")

	_, err := dp.Create(true, false, 4, nil, "test-label")
	require.NoError(t, err)

	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_PartyIndexSync, broadcasts[0].MsgType)
}

func TestDistributedPartyRegistryNodeLeaveCleanup(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockPartyRegistry{}
	logger := zap.NewNop()

	dp := NewDistributedPartyRegistry(logger, local, cluster, "node1")

	// Add remote index entries
	entries := []*distributedPartyIndexEntry{
		{PartyID: "party1.node2", Node: "node2", Open: true},
	}
	payload, _ := json.Marshal(entries)
	cluster.simulateMessage(ClusterMessageType_PartyIndexSync, "node2", payload)

	assert.Equal(t, 1, dp.RemoteCount())

	// Simulate node leave
	cluster.simulateNodeLeave("node2")

	assert.Equal(t, 0, dp.RemoteCount())
}

func TestDistributedPartyRegistryCountIncludesRemote(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockPartyRegistry{count: 3}
	logger := zap.NewNop()

	dp := NewDistributedPartyRegistry(logger, local, cluster, "node1")

	entries := []*distributedPartyIndexEntry{
		{PartyID: "party1.node2", Node: "node2"},
		{PartyID: "party2.node2", Node: "node2"},
	}
	payload, _ := json.Marshal(entries)
	cluster.simulateMessage(ClusterMessageType_PartyIndexSync, "node2", payload)

	assert.Equal(t, 5, dp.Count())
}

func TestDistributedPartyRegistryDelegatesInit(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockPartyRegistry{}
	logger := zap.NewNop()

	dp := NewDistributedPartyRegistry(logger, local, cluster, "node1")
	dp.Init(nil, nil, nil, nil)

	assert.True(t, local.initCalled)
}

func TestDistributedPartyRegistryDeleteBroadcasts(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockPartyRegistry{}
	logger := zap.NewNop()

	dp := NewDistributedPartyRegistry(logger, local, cluster, "node1")

	id := uuid.Must(uuid.NewV4())
	dp.Delete(id)

	require.Len(t, local.deleteCalled, 1)
	assert.Equal(t, id, local.deleteCalled[0])
}
