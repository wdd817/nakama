package server

import (
	"sync"
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// mockMessageRouter captures calls to the local message router.
type mockMessageRouter struct {
	mu                sync.Mutex
	sentToPresenceIDs []mockRouterSend
	sentToAll         []mockRouterBroadcast
}

type mockRouterSend struct {
	PresenceIDs []*PresenceID
	Envelope    *rtapi.Envelope
	Reliable    bool
}

type mockRouterBroadcast struct {
	Envelope *rtapi.Envelope
	Reliable bool
}

func (m *mockMessageRouter) SendToPresenceIDs(logger *zap.Logger, presenceIDs []*PresenceID, envelope *rtapi.Envelope, reliable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentToPresenceIDs = append(m.sentToPresenceIDs, mockRouterSend{
		PresenceIDs: presenceIDs,
		Envelope:    envelope,
		Reliable:    reliable,
	})
}

func (m *mockMessageRouter) SendToStream(logger *zap.Logger, stream PresenceStream, envelope *rtapi.Envelope, reliable bool) {
}

func (m *mockMessageRouter) SendDeferred(logger *zap.Logger, messages []*DeferredMessage) {
}

func (m *mockMessageRouter) SendToAll(logger *zap.Logger, envelope *rtapi.Envelope, reliable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentToAll = append(m.sentToAll, mockRouterBroadcast{Envelope: envelope, Reliable: reliable})
}

func (m *mockMessageRouter) getSentToPresenceIDs() []mockRouterSend {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mockRouterSend, len(m.sentToPresenceIDs))
	copy(result, m.sentToPresenceIDs)
	return result
}

func (m *mockMessageRouter) getSentToAll() []mockRouterBroadcast {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mockRouterBroadcast, len(m.sentToAll))
	copy(result, m.sentToAll)
	return result
}

func TestDistributedRouterLocalDelivery(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	localRouter := &mockMessageRouter{}
	logger := zap.NewNop()

	dr := NewDistributedMessageRouter(logger, localRouter, cluster, "node1")

	localPresence := &PresenceID{Node: "node1", SessionID: uuid.Must(uuid.NewV4())}
	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 1, Message: "test"}}}

	dr.SendToPresenceIDs(logger, []*PresenceID{localPresence}, envelope, true)

	sends := localRouter.getSentToPresenceIDs()
	require.Len(t, sends, 1)
	assert.Len(t, sends[0].PresenceIDs, 1)
	assert.Equal(t, localPresence.SessionID, sends[0].PresenceIDs[0].SessionID)

	// No cluster sends for local-only delivery
	cluster.mu.Lock()
	assert.Empty(t, cluster.sends)
	cluster.mu.Unlock()
}

func TestDistributedRouterRemoteForward(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	localRouter := &mockMessageRouter{}
	logger := zap.NewNop()

	dr := NewDistributedMessageRouter(logger, localRouter, cluster, "node1")

	remotePresence := &PresenceID{Node: "node2", SessionID: uuid.Must(uuid.NewV4())}
	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 2, Message: "remote"}}}

	dr.SendToPresenceIDs(logger, []*PresenceID{remotePresence}, envelope, true)

	// No local delivery
	sends := localRouter.getSentToPresenceIDs()
	assert.Empty(t, sends)

	// Should have sent to cluster
	cluster.mu.Lock()
	require.Len(t, cluster.sends, 1)
	assert.Equal(t, "node2", cluster.sends[0].NodeName)
	assert.Equal(t, ClusterMessageType_Envelope, cluster.sends[0].MsgType)
	cluster.mu.Unlock()
}

func TestDistributedRouterMixedDelivery(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2", "node3")
	localRouter := &mockMessageRouter{}
	logger := zap.NewNop()

	dr := NewDistributedMessageRouter(logger, localRouter, cluster, "node1")

	localPresence := &PresenceID{Node: "node1", SessionID: uuid.Must(uuid.NewV4())}
	remote2Presence := &PresenceID{Node: "node2", SessionID: uuid.Must(uuid.NewV4())}
	remote3Presence := &PresenceID{Node: "node3", SessionID: uuid.Must(uuid.NewV4())}
	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 3, Message: "mixed"}}}

	dr.SendToPresenceIDs(logger, []*PresenceID{localPresence, remote2Presence, remote3Presence}, envelope, true)

	// Local delivery should have 1 presence
	sends := localRouter.getSentToPresenceIDs()
	require.Len(t, sends, 1)
	assert.Len(t, sends[0].PresenceIDs, 1)

	// Remote delivery should have 2 sends (one per remote node)
	cluster.mu.Lock()
	assert.Len(t, cluster.sends, 2)
	cluster.mu.Unlock()
}

func TestDistributedRouterSendToAll(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	localRouter := &mockMessageRouter{}
	logger := zap.NewNop()

	dr := NewDistributedMessageRouter(logger, localRouter, cluster, "node1")

	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 4, Message: "broadcast"}}}

	dr.SendToAll(logger, envelope, true)

	// Local delivery
	allSends := localRouter.getSentToAll()
	require.Len(t, allSends, 1)

	// Cluster broadcast
	broadcasts := cluster.getBroadcasts()
	require.Len(t, broadcasts, 1)
	assert.Equal(t, ClusterMessageType_EnvelopeBroadcast, broadcasts[0].MsgType)
}

func TestDistributedRouterReceiveRemoteEnvelope(t *testing.T) {
	cluster := newMockClusterClient("node1", "node2")
	localRouter := &mockMessageRouter{}
	logger := zap.NewNop()

	_ = NewDistributedMessageRouter(logger, localRouter, cluster, "node1")

	// Simulate receiving a remote envelope
	sessionID := uuid.Must(uuid.NewV4())
	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 5, Message: "from-remote"}}}
	envBytes, _ := proto.Marshal(envelope)

	payload := encodeEnvelopePayload([]*PresenceID{{Node: "node1", SessionID: sessionID}}, envBytes, true)
	cluster.simulateMessage(ClusterMessageType_Envelope, "node2", payload)

	// Should have been delivered locally
	sends := localRouter.getSentToPresenceIDs()
	require.Len(t, sends, 1)
	assert.Len(t, sends[0].PresenceIDs, 1)
	assert.Equal(t, sessionID, sends[0].PresenceIDs[0].SessionID)
}

// envelopePayload is the wire format for forwarded envelopes.
type testEnvelopePayload struct {
	PresenceIDs []testPresenceIDWire `json:"p"`
	Envelope    []byte               `json:"e"`
	Reliable    bool                 `json:"r"`
}

type testPresenceIDWire struct {
	Node      string    `json:"n"`
	SessionID uuid.UUID `json:"s"`
}
