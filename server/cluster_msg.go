package server

import (
	"errors"
	"fmt"

	"github.com/gofrs/uuid/v5"
)

type ClusterMessageType uint8

const (
	ClusterMessageType_PresenceEvent ClusterMessageType = iota + 1
	ClusterMessageType_MatchRPC
	ClusterMessageType_MatchmakerSync
	ClusterMessageType_MatchmakerRemove
	ClusterMessageType_PartyRPC
	ClusterMessageType_MatchIndexSync
	ClusterMessageType_PartyIndexSync
	ClusterMessageType_StatusRequest
	ClusterMessageType_Envelope
	ClusterMessageType_EnvelopeBroadcast
	ClusterMessageType_StreamRPC
	ClusterMessageType_Request
	ClusterMessageType_Response
)

// ClusterMessage is the wire format for all inter-node messages.
// Binary layout: [type:1byte][node_len:1byte][node:variable][payload:rest]
type ClusterMessage struct {
	Type       ClusterMessageType
	SourceNode string
	Payload    []byte
	RequestID  uuid.UUID // populated only for request messages, used by Reply()
}

func (m *ClusterMessage) Encode() []byte {
	nodeBytes := []byte(m.SourceNode)
	nodeLen := len(nodeBytes)
	buf := make([]byte, 1+1+nodeLen+len(m.Payload))
	buf[0] = byte(m.Type)
	buf[1] = byte(nodeLen)
	copy(buf[2:], nodeBytes)
	copy(buf[2+nodeLen:], m.Payload)
	return buf
}

func DecodeClusterMessage(data []byte) (*ClusterMessage, error) {
	if len(data) < 2 {
		return nil, errors.New("cluster message too short")
	}
	msgType := ClusterMessageType(data[0])
	nodeLen := int(data[1])
	if len(data) < 2+nodeLen {
		return nil, fmt.Errorf("cluster message truncated: need %d bytes for node, have %d", nodeLen, len(data)-2)
	}
	return &ClusterMessage{
		Type:       msgType,
		SourceNode: string(data[2 : 2+nodeLen]),
		Payload:    data[2+nodeLen:],
	}, nil
}

// Request wire format: [reqID:16bytes][payload:rest]

func EncodeClusterRequest(reqID uuid.UUID, payload []byte) []byte {
	buf := make([]byte, 16+len(payload))
	copy(buf[:16], reqID.Bytes())
	copy(buf[16:], payload)
	return buf
}

func DecodeClusterRequest(data []byte) (uuid.UUID, []byte, error) {
	if len(data) < 16 {
		return uuid.Nil, nil, errors.New("cluster request too short for UUID")
	}
	id, err := uuid.FromBytes(data[:16])
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("cluster request invalid UUID: %w", err)
	}
	return id, data[16:], nil
}
