package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// mockMatchRegistry implements MatchRegistry for testing.
type mockMatchRegistry struct {
	mu             sync.Mutex
	matches        map[string]*api.Match
	matchLabels    map[string]string
	indexEntries   []*MatchIndexEntry
	joinAttempts   []string
	sendDataCalls  []string
	signalCalls    []string
	getStateCalls  []string
	removedMatches []uuid.UUID
	count          int
}

func newMockMatchRegistry() *mockMatchRegistry {
	return &mockMatchRegistry{
		matches:     make(map[string]*api.Match),
		matchLabels: make(map[string]string),
	}
}

func (m *mockMatchRegistry) CreateMatch(ctx context.Context, createFn RuntimeMatchCreateFunction, module string, params map[string]interface{}) (string, error) {
	id := uuid.Must(uuid.NewV4())
	idStr := id.String() + ".node1"
	m.mu.Lock()
	m.matches[idStr] = &api.Match{MatchId: idStr, Label: &wrapperspb.StringValue{Value: ""}}
	m.count++
	m.mu.Unlock()
	return idStr, nil
}

func (m *mockMatchRegistry) NewMatch(logger *zap.Logger, id uuid.UUID, core RuntimeMatchCore, stopped *atomic.Bool, params map[string]interface{}) (*MatchHandler, error) {
	return nil, nil
}

func (m *mockMatchRegistry) GetMatch(ctx context.Context, id string) (*api.Match, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if match, ok := m.matches[id]; ok {
		return match, "", nil
	}
	return nil, "", nil
}

func (m *mockMatchRegistry) RemoveMatch(id uuid.UUID, stream PresenceStream) {
	m.mu.Lock()
	m.removedMatches = append(m.removedMatches, id)
	m.mu.Unlock()
}

func (m *mockMatchRegistry) UpdateMatchLabel(id uuid.UUID, tickRate int, handlerName, label string, createTime int64) error {
	m.mu.Lock()
	m.matchLabels[id.String()] = label
	m.mu.Unlock()
	return nil
}

func (m *mockMatchRegistry) ClearMatchLabel(id uuid.UUID, node string) error {
	return nil
}

func (m *mockMatchRegistry) ListMatches(ctx context.Context, limit int, authoritative *wrapperspb.BoolValue, label *wrapperspb.StringValue, minSize *wrapperspb.Int32Value, maxSize *wrapperspb.Int32Value, query *wrapperspb.StringValue, node *wrapperspb.StringValue) ([]*api.Match, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*api.Match
	for _, match := range m.matches {
		result = append(result, match)
	}
	return result, nil, nil
}

func (m *mockMatchRegistry) Stop(graceSeconds int) chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (m *mockMatchRegistry) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *mockMatchRegistry) JoinAttempt(ctx context.Context, id uuid.UUID, node string, userID, sessionID uuid.UUID, username string, sessionExpiry int64, vars map[string]string, clientIP, clientPort, fromNode string, metadata map[string]string) (bool, bool, bool, string, string, []*MatchPresence) {
	m.mu.Lock()
	m.joinAttempts = append(m.joinAttempts, id.String())
	m.mu.Unlock()
	return true, true, true, "", "", nil
}

func (m *mockMatchRegistry) Join(id uuid.UUID, presences []*MatchPresence) {}

func (m *mockMatchRegistry) Leave(id uuid.UUID, presences []*MatchPresence) {}

func (m *mockMatchRegistry) Kick(stream PresenceStream, presences []*MatchPresence) {}

func (m *mockMatchRegistry) SendData(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string, opCode int64, data []byte, reliable bool, receiveTime int64) {
	m.mu.Lock()
	m.sendDataCalls = append(m.sendDataCalls, id.String())
	m.mu.Unlock()
}

func (m *mockMatchRegistry) Signal(ctx context.Context, id, data string) (string, error) {
	m.mu.Lock()
	m.signalCalls = append(m.signalCalls, id)
	m.mu.Unlock()
	return "ok", nil
}

func (m *mockMatchRegistry) GetState(ctx context.Context, id uuid.UUID, node string) ([]*rtapi.UserPresence, int64, string, error) {
	m.mu.Lock()
	m.getStateCalls = append(m.getStateCalls, id.String())
	m.mu.Unlock()
	return nil, 0, "", nil
}

// --- Tests ---

func TestDistributedMatchRegistryCreateBroadcastsIndex(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := newMockMatchRegistry()
	logger := zap.NewNop()

	dm := NewDistributedMatchRegistry(logger, local, cluster, "node1")
	defer dm.Stop(0)

	_, err := dm.CreateMatch(context.Background(), nil, "test", nil)
	require.NoError(t, err)

	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_MatchIndexSync, broadcasts[0].MsgType)
}

func TestDistributedMatchRegistryGetMatchLocal(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := newMockMatchRegistry()
	logger := zap.NewNop()

	dm := NewDistributedMatchRegistry(logger, local, cluster, "node1")
	defer dm.Stop(0)

	// Add a local match
	matchID := uuid.Must(uuid.NewV4()).String() + ".node1"
	local.mu.Lock()
	local.matches[matchID] = &api.Match{MatchId: matchID, Label: &wrapperspb.StringValue{Value: "test"}}
	local.mu.Unlock()

	match, _, err := dm.GetMatch(context.Background(), matchID)
	require.NoError(t, err)
	require.NotNil(t, match)
	assert.Equal(t, matchID, match.MatchId)
}

func TestDistributedMatchRegistryGetMatchRemote(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := newMockMatchRegistry()
	logger := zap.NewNop()

	// Set up cluster to respond to requests
	cluster.requestHandler = func(nodeName string, msgType ClusterMessageType, payload []byte) ([]byte, error) {
		resp := &api.Match{MatchId: "remote-match.node2", Label: &wrapperspb.StringValue{Value: "remote"}}
		data, _ := json.Marshal(resp)
		return data, nil
	}

	dm := NewDistributedMatchRegistry(logger, local, cluster, "node1")
	defer dm.Stop(0)

	match, _, err := dm.GetMatch(context.Background(), "some-uuid.node2")
	require.NoError(t, err)
	require.NotNil(t, match)
	assert.Equal(t, "remote-match.node2", match.MatchId)
}

func TestDistributedMatchRegistryNodeLeaveCleanup(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := newMockMatchRegistry()
	logger := zap.NewNop()

	dm := NewDistributedMatchRegistry(logger, local, cluster, "node1")
	defer dm.Stop(0)

	// Add remote index entries
	entries := []*distributedMatchIndexEntry{
		{MatchID: "match1.node2", Node: "node2", HandlerName: "test"},
	}
	payload, _ := json.Marshal(entries)
	cluster.simulateMessage(ClusterMessageType_MatchIndexSync, "node2", payload)

	// Verify remote entries exist
	assert.Equal(t, 1, dm.RemoteCount())

	// Simulate node leave
	cluster.simulateNodeLeave("node2")

	// Verify remote entries cleared
	assert.Equal(t, 0, dm.RemoteCount())
}

func TestDistributedMatchRegistryCountIncludesRemote(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	local := newMockMatchRegistry()
	logger := zap.NewNop()

	dm := NewDistributedMatchRegistry(logger, local, cluster, "node1")
	defer dm.Stop(0)

	// Add a local match
	local.mu.Lock()
	local.count = 2
	local.mu.Unlock()

	// Add remote index entries
	entries := []*distributedMatchIndexEntry{
		{MatchID: "match1.node2", Node: "node2"},
		{MatchID: "match2.node2", Node: "node2"},
		{MatchID: "match3.node2", Node: "node2"},
	}
	payload, _ := json.Marshal(entries)
	cluster.simulateMessage(ClusterMessageType_MatchIndexSync, "node2", payload)

	assert.Equal(t, 5, dm.Count())
}
