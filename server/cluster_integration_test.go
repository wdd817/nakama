package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestClusterIntegration_PresenceSync(t *testing.T) {
	// 1. Create two Cluster instances (random ports)
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2 && c2.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// 2. Create DistributedTracker on each
	dt1 := NewDistributedTracker(c1, "node1")
	dt2 := NewDistributedTracker(c2, "node2")

	// 3. Track a presence on node1 (broadcast to node2)
	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 4, Subject: uuid.Must(uuid.NewV4())}
	meta := PresenceMeta{Username: "player1", Status: "online"}

	dt1.TrackRemotePresence(sessionID, stream, userID, meta, "node1")

	// 4. Wait for gossip propagation
	require.Eventually(t, func() bool {
		return len(dt2.ListByStream(stream, true, true)) == 1
	}, 5*time.Second, 100*time.Millisecond, "node2 should see node1's presence")

	// 5. Verify node2 sees the presence
	presences := dt2.ListByStream(stream, true, true)
	require.Len(t, presences, 1)
	assert.Equal(t, "node1", presences[0].ID.Node)
	assert.Equal(t, sessionID, presences[0].ID.SessionID)
	assert.Equal(t, userID, presences[0].UserID)
	assert.Equal(t, "player1", presences[0].Meta.Username)

	// 6. Untrack on node1
	dt1.UntrackRemotePresence(sessionID, stream, userID, "node1")

	// 7. Verify node2 no longer sees it
	require.Eventually(t, func() bool {
		return len(dt2.ListByStream(stream, true, true)) == 0
	}, 5*time.Second, 100*time.Millisecond, "node2 should no longer see node1's presence")
}

func TestClusterIntegration_MessageRouting(t *testing.T) {
	// 1. Create two Cluster instances
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2 && c2.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// 2. Create mock local routers on each node
	localRouter1 := &mockMessageRouter{}
	localRouter2 := &mockMessageRouter{}
	logger := zap.NewNop()

	_ = NewDistributedMessageRouter(logger, localRouter1, c1, "node1")
	dr2 := NewDistributedMessageRouter(logger, localRouter2, c2, "node2")
	_ = dr2

	// 3. Create a presence targeting node2
	sessionID := uuid.Must(uuid.NewV4())
	presenceID := &PresenceID{Node: "node2", SessionID: sessionID}

	// 4. From node1's distributed router, send to the node2 presence
	envelope := &rtapi.Envelope{Message: &rtapi.Envelope_Error{Error: &rtapi.Error{Code: 42, Message: "cross-node"}}}

	// Manually encode and send via c1 (simulating what DistributedMessageRouter does)
	envBytes, err := proto.Marshal(envelope)
	require.NoError(t, err)
	payload := encodeEnvelopePayload([]*PresenceID{presenceID}, envBytes, true)
	err = c1.SendTo("node2", ClusterMessageType_Envelope, payload)
	require.NoError(t, err)

	// 5. Verify node2's local router received the envelope
	require.Eventually(t, func() bool {
		sends := localRouter2.getSentToPresenceIDs()
		return len(sends) > 0
	}, 5*time.Second, 100*time.Millisecond, "node2 should receive the forwarded envelope")

	sends := localRouter2.getSentToPresenceIDs()
	require.Len(t, sends, 1)
	assert.Len(t, sends[0].PresenceIDs, 1)
	assert.Equal(t, sessionID, sends[0].PresenceIDs[0].SessionID)
}

func TestClusterIntegration_MatchmakerSync(t *testing.T) {
	// 1. Create two Cluster instances
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2 && c2.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// 2. Create mock matchmakers
	local1 := &mockMatchmaker{}
	local2 := &mockMatchmaker{}
	logger := zap.NewNop()

	_ = NewDistributedMatchmaker(logger, local1, c1, "node1", 10*time.Second)
	dm2 := NewDistributedMatchmaker(logger, local2, c2, "node2", 10*time.Second)
	defer dm2.Stop()

	// 3. Broadcast a matchmaker sync from node1
	extract := &MatchmakerExtract{
		Presences: []*MatchmakerPresence{{UserId: "u1", SessionId: "s1", Node: "node1"}},
		SessionID: "s1",
		Query:     "*",
		MinCount:  2,
		MaxCount:  4,
		Ticket:    "ticket-1",
		Node:      "node1",
	}
	payload, _ := json.Marshal([]*MatchmakerExtract{extract})
	c1.Broadcast(ClusterMessageType_MatchmakerSync, payload)

	// 4. Verify node2's local matchmaker received the insert
	require.Eventually(t, func() bool {
		return local2.insertCalled > 0
	}, 5*time.Second, 100*time.Millisecond, "node2 should receive matchmaker sync")
}

func TestClusterIntegration_NodeFailure(t *testing.T) {
	// 1. Create two Cluster instances
	c1 := newTestCluster(t, "node1", nil)

	logger := zap.NewNop()
	cfg := &ClusterConfig{
		Enabled:        true,
		GossipPort:     0,
		JoinAddrs:      []string{c1.LocalAddr()},
		MetaInterval:   5 * time.Second,
		RequestTimeout: 5 * time.Second,
	}
	c2, err := NewCluster(logger, "node2", cfg)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2 && c2.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// 2. Create distributed tracker on node1
	dt1 := NewDistributedTracker(c1, "node1")

	// 3. Simulate a remote presence from node2
	sessionID := uuid.Must(uuid.NewV4())
	userID := uuid.Must(uuid.NewV4())
	stream := PresenceStream{Mode: 4, Subject: uuid.Must(uuid.NewV4())}
	event := &distributedPresenceEvent{
		Join:      true,
		Node:      "node2",
		SessionID: sessionID,
		UserID:    userID,
		Stream:    stream,
		Meta:      PresenceMeta{Username: "player2"},
	}
	eventPayload, _ := json.Marshal(event)
	// Deliver directly to dt1's handler (simulating cluster message)
	msg := &ClusterMessage{
		Type:       ClusterMessageType_PresenceEvent,
		SourceNode: "node2",
		Payload:    eventPayload,
	}
	dt1.handlePresenceEvent(msg)

	require.Len(t, dt1.ListByStream(stream, true, true), 1)

	// 4. Track node leave callback
	var leftNode string
	var leftMu sync.Mutex
	done := make(chan struct{})
	originalLeave := dt1.handleNodeLeave
	c1.OnNodeLeave(func(name string) {
		leftMu.Lock()
		leftNode = name
		leftMu.Unlock()
		originalLeave(name)
		close(done)
	})

	// 5. Stop node2
	c2.Stop()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for node leave callback")
	}

	leftMu.Lock()
	assert.Equal(t, "node2", leftNode)
	leftMu.Unlock()

	// 6. Verify remote presences from node2 are cleared
	assert.Empty(t, dt1.ListByStream(stream, true, true))
	assert.Equal(t, 0, dt1.Count())
}
