package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClusterConfigDefaults(t *testing.T) {
	cfg := NewClusterConfig()
	assert.False(t, cfg.Enabled)
	assert.Equal(t, 7352, cfg.GossipPort)
	assert.Empty(t, cfg.JoinAddrs)
	assert.Equal(t, 5*time.Second, cfg.MetaInterval)
	assert.Equal(t, 10*time.Second, cfg.MatchmakerSyncInterval)
	assert.Equal(t, 10*time.Second, cfg.MatchIndexSyncInterval)
	assert.Equal(t, 5*time.Second, cfg.RequestTimeout)
}
