package server

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newTestCluster(t *testing.T, name string, joinAddrs []string) *Cluster {
	t.Helper()
	logger := zap.NewNop()
	cfg := &ClusterConfig{
		Enabled:        true,
		GossipPort:     0, // random port
		JoinAddrs:      joinAddrs,
		MetaInterval:   5 * time.Second,
		RequestTimeout: 5 * time.Second,
	}
	c, err := NewCluster(logger, name, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { c.Stop() })
	return c
}

func TestClusterTwoNodeJoin(t *testing.T) {
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	// Wait for gossip convergence
	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2 && c2.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	names1 := c1.MemberNames()
	names2 := c2.MemberNames()
	assert.Contains(t, names1, "node1")
	assert.Contains(t, names1, "node2")
	assert.Contains(t, names2, "node1")
	assert.Contains(t, names2, "node2")
}

func TestClusterSendTo(t *testing.T) {
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	var received []byte
	var receivedFrom string
	var mu sync.Mutex
	done := make(chan struct{})

	c2.RegisterHandler(ClusterMessageType_PresenceEvent, func(msg *ClusterMessage) {
		mu.Lock()
		received = msg.Payload
		receivedFrom = msg.SourceNode
		mu.Unlock()
		close(done)
	})

	err := c1.SendTo("node2", ClusterMessageType_PresenceEvent, []byte("hello"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []byte("hello"), received)
	assert.Equal(t, "node1", receivedFrom)
}

func TestClusterRequestReply(t *testing.T) {
	c1 := newTestCluster(t, "node1", nil)
	c2 := newTestCluster(t, "node2", []string{c1.LocalAddr()})

	require.Eventually(t, func() bool {
		return c1.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// Node1 handles requests by echoing back with prefix
	c1.RegisterHandler(ClusterMessageType_MatchRPC, func(msg *ClusterMessage) {
		response := append([]byte("echo:"), msg.Payload...)
		c1.Reply(msg, response)
	})

	// Node2 sends request to Node1
	resp, err := c2.Request("node1", ClusterMessageType_MatchRPC, []byte("ping"))
	require.NoError(t, err)
	assert.Equal(t, []byte("echo:ping"), resp)
}

func TestClusterNodeLeaveCallback(t *testing.T) {
	c1 := newTestCluster(t, "node1", nil)

	// Create c2 manually so we can stop it without cleanup
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
		return c1.MemberCount() == 2
	}, 5*time.Second, 100*time.Millisecond)

	var leftNode string
	done := make(chan struct{})
	c1.OnNodeLeave(func(name string) {
		leftNode = name
		close(done)
	})

	c2.Stop()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for node leave callback")
	}

	assert.Equal(t, "node2", leftNode)
}
