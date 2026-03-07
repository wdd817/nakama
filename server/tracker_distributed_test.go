package server

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockClusterClient implements ClusterClient for testing distributed components.
type mockClusterClient struct {
	mu             sync.Mutex
	broadcasts     []mockBroadcast
	sends          []mockSend
	handlers       map[ClusterMessageType]ClusterMessageHandler
	nodeLeaveFunc  func(string)
	members        []string
	requestHandler func(nodeName string, msgType ClusterMessageType, payload []byte) ([]byte, error)
}

type mockBroadcast struct {
	MsgType ClusterMessageType
	Payload []byte
}

type mockSend struct {
	NodeName string
	MsgType  ClusterMessageType
	Payload  []byte
}

func newMockClusterClient(members ...string) *mockClusterClient {
	return &mockClusterClient{
		handlers: make(map[ClusterMessageType]ClusterMessageHandler),
		members:  members,
	}
}

func (m *mockClusterClient) Broadcast(msgType ClusterMessageType, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcasts = append(m.broadcasts, mockBroadcast{MsgType: msgType, Payload: payload})
}

func (m *mockClusterClient) SendTo(nodeName string, msgType ClusterMessageType, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, mockSend{NodeName: nodeName, MsgType: msgType, Payload: payload})
	return nil
}

func (m *mockClusterClient) Request(nodeName string, msgType ClusterMessageType, payload []byte) ([]byte, error) {
	if m.requestHandler != nil {
		return m.requestHandler(nodeName, msgType, payload)
	}
	return nil, nil
}

func (m *mockClusterClient) Reply(msg *ClusterMessage, response []byte) {}

func (m *mockClusterClient) RegisterHandler(msgType ClusterMessageType, handler ClusterMessageHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[msgType] = handler
}

func (m *mockClusterClient) OnNodeLeave(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeLeaveFunc = fn
}

func (m *mockClusterClient) MemberNames() []string {
	return m.members
}

func (m *mockClusterClient) getBroadcasts() []mockBroadcast {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mockBroadcast, len(m.broadcasts))
	copy(result, m.broadcasts)
	return result
}

func (m *mockClusterClient) simulateMessage(msgType ClusterMessageType, sourceNode string, payload []byte) {
	m.mu.Lock()
	handler, ok := m.handlers[msgType]
	m.mu.Unlock()
	if ok {
		handler(&ClusterMessage{
			Type:       msgType,
			SourceNode: sourceNode,
			Payload:    payload,
		})
	}
}

func (m *mockClusterClient) simulateNodeLeave(nodeName string) {
	m.mu.Lock()
	fn := m.nodeLeaveFunc
	m.mu.Unlock()
	if fn != nil {
		fn(nodeName)
	}
}

// --- Tests ---

func TestDistributedTrackerTrackBroadcasts(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "testuser", Status: "online"}

	dt.TrackRemotePresence(sessionID, stream, userID, meta, "node1")

	broadcasts := mock.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_PresenceEvent, broadcasts[0].MsgType)

	// Decode the broadcast payload
	var event distributedPresenceEvent
	err := json.Unmarshal(broadcasts[0].Payload, &event)
	require.NoError(t, err)
	assert.True(t, event.Join)
	assert.Equal(t, "node1", event.Node)
	assert.Equal(t, sessionID, event.SessionID)
	assert.Equal(t, userID, event.UserID)
}

func TestDistributedTrackerRemotePresence(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "remoteuser", Status: "online"}

	// Simulate receiving a remote presence join
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      "node2",
		SessionID: sessionID,
		UserID:    userID,
		Stream:    stream,
		Meta:      meta,
	}
	payload, _ := json.Marshal(event)
	mock.simulateMessage(ClusterMessageType_PresenceEvent, "node2", payload)

	// Verify remote presence is visible
	presences := dt.ListByStream(stream, true, true)
	require.Len(t, presences, 1)
	assert.Equal(t, "node2", presences[0].ID.Node)
	assert.Equal(t, sessionID, presences[0].ID.SessionID)
	assert.Equal(t, userID, presences[0].UserID)
	assert.Equal(t, "remoteuser", presences[0].Meta.Username)
}

func TestDistributedTrackerUntrackBroadcasts(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "testuser"}

	// Track first
	dt.TrackRemotePresence(sessionID, stream, userID, meta, "node1")

	// Untrack
	dt.UntrackRemotePresence(sessionID, stream, userID, "node1")

	broadcasts := mock.getBroadcasts()
	require.Len(t, broadcasts, 2)
	assert.Equal(t, ClusterMessageType_PresenceEvent, broadcasts[1].MsgType)

	var event distributedPresenceEvent
	err := json.Unmarshal(broadcasts[1].Payload, &event)
	require.NoError(t, err)
	assert.False(t, event.Join)
}

func TestDistributedTrackerNodeLeave(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "remoteuser"}

	// Add remote presence from node2
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      "node2",
		SessionID: sessionID,
		UserID:    userID,
		Stream:    stream,
		Meta:      meta,
	}
	payload, _ := json.Marshal(event)
	mock.simulateMessage(ClusterMessageType_PresenceEvent, "node2", payload)

	// Verify it exists
	require.Len(t, dt.ListByStream(stream, true, true), 1)

	// Simulate node2 leaving
	mock.simulateNodeLeave("node2")

	// Verify remote presences are cleared
	assert.Empty(t, dt.ListByStream(stream, true, true))
}

func TestDistributedTrackerCountIncludesRemote(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}

	// Add a remote presence
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      "node2",
		SessionID: uuid.Must(uuid.NewV4()),
		UserID:    uuid.Must(uuid.NewV4()),
		Stream:    stream,
		Meta:      PresenceMeta{Username: "user1"},
	}
	payload, _ := json.Marshal(event)
	mock.simulateMessage(ClusterMessageType_PresenceEvent, "node2", payload)

	assert.Equal(t, 1, dt.CountByStream(stream))
}

func TestDistributedTrackerListLocalOnly(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}

	// Add a remote presence
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      "node2",
		SessionID: uuid.Must(uuid.NewV4()),
		UserID:    uuid.Must(uuid.NewV4()),
		Stream:    stream,
		Meta:      PresenceMeta{Username: "remoteuser"},
	}
	payload, _ := json.Marshal(event)
	mock.simulateMessage(ClusterMessageType_PresenceEvent, "node2", payload)

	// ListByStream should include remote
	all := dt.ListByStream(stream, true, true)
	assert.Len(t, all, 1)

	// ListLocalByStream should NOT include remote
	local := dt.ListLocalByStream(stream)
	assert.Empty(t, local)
}

func TestDistributedTrackerUntrackAllBroadcasts(t *testing.T) {
	mock := newMockClusterClient("node1", "node2")
	dt := NewDistributedTracker(mock, "node1")

	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 0, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "testuser"}

	// Track
	dt.TrackRemotePresence(sessionID, stream, userID, meta, "node1")

	// UntrackAll
	dt.UntrackAllRemotePresences(sessionID, "node1")

	broadcasts := mock.getBroadcasts()
	require.Len(t, broadcasts, 2)

	var event distributedPresenceEvent
	err := json.Unmarshal(broadcasts[1].Payload, &event)
	require.NoError(t, err)
	assert.False(t, event.Join)
	assert.True(t, event.UntrackAll)
	assert.Equal(t, sessionID, event.SessionID)
}
