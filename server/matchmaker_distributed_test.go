package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockMatchmaker implements Matchmaker for testing.
type mockMatchmaker struct {
	addCalled       int
	insertCalled    int
	extractResult   []*MatchmakerExtract
	removeCalled    []string
	removeAllCalled []string
	onMatchedFn     func(entries [][]*MatchmakerEntry)
}

func (m *mockMatchmaker) Pause()  {}
func (m *mockMatchmaker) Resume() {}
func (m *mockMatchmaker) Stop()   {}
func (m *mockMatchmaker) OnMatchedEntries(fn func(entries [][]*MatchmakerEntry)) {
	m.onMatchedFn = fn
}
func (m *mockMatchmaker) OnStatsUpdate(fn func(stats *api.MatchmakerStats)) {}
func (m *mockMatchmaker) Add(ctx context.Context, presences []*MatchmakerPresence, sessionID, partyId, query string, minCount, maxCount, countMultiple int, stringProperties map[string]string, numericProperties map[string]float64) (string, int64, error) {
	m.addCalled++
	return "ticket-1", time.Now().Unix(), nil
}
func (m *mockMatchmaker) Insert(extracts []*MatchmakerExtract) error {
	m.insertCalled++
	return nil
}
func (m *mockMatchmaker) Extract() []*MatchmakerExtract {
	return m.extractResult
}
func (m *mockMatchmaker) RemoveSession(sessionID, ticket string) error {
	return nil
}
func (m *mockMatchmaker) RemoveSessionAll(sessionID string) error {
	return nil
}
func (m *mockMatchmaker) RemoveParty(partyID, ticket string) error {
	return nil
}
func (m *mockMatchmaker) RemovePartyAll(partyID string) error {
	return nil
}
func (m *mockMatchmaker) RemoveAll(node string) {
	m.removeAllCalled = append(m.removeAllCalled, node)
}
func (m *mockMatchmaker) Remove(tickets []string) {
	m.removeCalled = append(m.removeCalled, tickets...)
}
func (m *mockMatchmaker) GetStats() *api.MatchmakerStats {
	return nil
}
func (m *mockMatchmaker) SetStats(stats *api.MatchmakerStats) {}

func TestDistributedMatchmakerAddBroadcasts(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockMatchmaker{}
	logger := zap.NewNop()

	dm := NewDistributedMatchmaker(logger, local, cluster, "node1", 10*time.Second)
	defer dm.Stop()

	_, _, err := dm.Add(context.Background(),
		[]*MatchmakerPresence{{UserId: "u1", SessionId: "s1", Node: "node1"}},
		"s1", "", "query", 2, 4, 1, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, local.addCalled)

	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_MatchmakerSync, broadcasts[0].MsgType)
}

func TestDistributedMatchmakerReceiveRemoteTicket(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockMatchmaker{}
	logger := zap.NewNop()

	dm := NewDistributedMatchmaker(logger, local, cluster, "node1", 10*time.Second)
	defer dm.Stop()

	extract := &MatchmakerExtract{
		Presences: []*MatchmakerPresence{{UserId: "u2", SessionId: "s2", Node: "node2"}},
		SessionID: "s2",
		Query:     "query",
		MinCount:  2,
		MaxCount:  4,
		Ticket:    "ticket-remote",
		Node:      "node2",
	}
	payload, _ := json.Marshal([]*MatchmakerExtract{extract})
	cluster.simulateMessage(ClusterMessageType_MatchmakerSync, "node2", payload)

	assert.Equal(t, 1, local.insertCalled)
}

func TestDistributedMatchmakerRemoveBroadcasts(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockMatchmaker{}
	logger := zap.NewNop()

	dm := NewDistributedMatchmaker(logger, local, cluster, "node1", 10*time.Second)
	defer dm.Stop()

	err := dm.RemoveSessionAll("session-1")
	require.NoError(t, err)

	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_MatchmakerRemove, broadcasts[0].MsgType)

	var removal distributedMatchmakerRemoval
	err = json.Unmarshal(broadcasts[0].Payload, &removal)
	require.NoError(t, err)
	assert.Equal(t, "session-1", removal.SessionID)
	assert.True(t, removal.All)
}

func TestDistributedMatchmakerNodeLeaveCleanup(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockMatchmaker{}
	logger := zap.NewNop()

	dm := NewDistributedMatchmaker(logger, local, cluster, "node1", 10*time.Second)
	defer dm.Stop()

	cluster.simulateNodeLeave("node2")

	require.Len(t, local.removeAllCalled, 1)
	assert.Equal(t, "node2", local.removeAllCalled[0])
}

func TestDistributedMatchmakerRemoveTicketsBroadcasts(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := &mockMatchmaker{}
	logger := zap.NewNop()

	dm := NewDistributedMatchmaker(logger, local, cluster, "node1", 10*time.Second)
	defer dm.Stop()

	dm.Remove([]string{"ticket-a", "ticket-b"})

	require.Len(t, local.removeCalled, 2)

	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_MatchmakerRemove, broadcasts[0].MsgType)
}
