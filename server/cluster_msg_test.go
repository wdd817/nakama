package server

import (
	"testing"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusterMessageEncodeDecode(t *testing.T) {
	original := &ClusterMessage{
		Type:       ClusterMessageType_PresenceEvent,
		SourceNode: "node-1",
		Payload:    []byte("hello world"),
	}

	encoded := original.Encode()
	decoded, err := DecodeClusterMessage(encoded)
	require.NoError(t, err)

	assert.Equal(t, original.Type, decoded.Type)
	assert.Equal(t, original.SourceNode, decoded.SourceNode)
	assert.Equal(t, original.Payload, decoded.Payload)
}

func TestClusterMessageEncodeDecodeEmptyPayload(t *testing.T) {
	original := &ClusterMessage{
		Type:       ClusterMessageType_MatchRPC,
		SourceNode: "node-2",
		Payload:    []byte{},
	}

	encoded := original.Encode()
	decoded, err := DecodeClusterMessage(encoded)
	require.NoError(t, err)

	assert.Equal(t, original.Type, decoded.Type)
	assert.Equal(t, original.SourceNode, decoded.SourceNode)
	assert.Empty(t, decoded.Payload)
}

func TestClusterRequestEncodeDecode(t *testing.T) {
	reqID := uuid.Must(uuid.NewV4())
	payload := []byte("request payload")

	encoded := EncodeClusterRequest(reqID, payload)
	decodedID, decodedPayload, err := DecodeClusterRequest(encoded)
	require.NoError(t, err)

	assert.Equal(t, reqID, decodedID)
	assert.Equal(t, payload, decodedPayload)
}

func TestClusterRequestEncodeDecodeEmptyPayload(t *testing.T) {
	reqID := uuid.Must(uuid.NewV4())

	encoded := EncodeClusterRequest(reqID, []byte{})
	decodedID, decodedPayload, err := DecodeClusterRequest(encoded)
	require.NoError(t, err)

	assert.Equal(t, reqID, decodedID)
	assert.Empty(t, decodedPayload)
}

func TestClusterMessageDecodeInvalid(t *testing.T) {
	// Too short - needs at least 2 bytes (type + node_len)
	_, err := DecodeClusterMessage([]byte{})
	assert.Error(t, err)

	_, err = DecodeClusterMessage([]byte{0x01})
	assert.Error(t, err)
}

func TestClusterRequestDecodeInvalid(t *testing.T) {
	// Too short - needs at least 16 bytes for UUID
	_, _, err := DecodeClusterRequest([]byte{1, 2, 3})
	assert.Error(t, err)
}
