package server

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/hashicorp/memberlist"
	"go.uber.org/zap"
)

type ClusterMessageHandler func(msg *ClusterMessage)

type Cluster struct {
	logger          *zap.Logger
	node            string
	config          *ClusterConfig
	ml              *memberlist.Memberlist
	handlers        map[ClusterMessageType]ClusterMessageHandler
	handlersMu      sync.RWMutex
	pendingRequests sync.Map // uuid.UUID -> chan []byte
	onNodeLeave     func(string)
	onNodeLeaveMu   sync.RWMutex
}

func NewCluster(logger *zap.Logger, nodeName string, config *ClusterConfig) (*Cluster, error) {
	c := &Cluster{
		logger:   logger,
		node:     nodeName,
		config:   config,
		handlers: make(map[ClusterMessageType]ClusterMessageHandler),
	}

	mlConfig := memberlist.DefaultLANConfig()
	mlConfig.Name = nodeName
	mlConfig.BindPort = config.GossipPort
	mlConfig.AdvertisePort = config.GossipPort
	mlConfig.Delegate = &clusterDelegate{cluster: c}
	mlConfig.Events = &clusterEventDelegate{cluster: c}
	mlConfig.LogOutput = newZapWriter(logger)

	ml, err := memberlist.Create(mlConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %w", err)
	}
	c.ml = ml

	if len(config.JoinAddrs) > 0 {
		resolved := resolveJoinAddrs(config.JoinAddrs)
		if len(resolved) > 0 {
			_, err := ml.Join(resolved)
			if err != nil {
				logger.Warn("Failed to join cluster seeds, will retry via gossip", zap.Error(err))
			}
		}
	}

	return c, nil
}

func (c *Cluster) Stop() {
	if c.ml != nil {
		_ = c.ml.Leave(5 * time.Second)
		_ = c.ml.Shutdown()
	}
}

func (c *Cluster) LocalAddr() string {
	if c.ml == nil {
		return ""
	}
	addr := c.ml.LocalNode().Addr
	port := c.ml.LocalNode().Port
	return net.JoinHostPort(addr.String(), strconv.Itoa(int(port)))
}

func (c *Cluster) MemberCount() int {
	return c.ml.NumMembers()
}

func (c *Cluster) MemberNames() []string {
	members := c.ml.Members()
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Name
	}
	return names
}

func (c *Cluster) RegisterHandler(msgType ClusterMessageType, handler ClusterMessageHandler) {
	c.handlersMu.Lock()
	c.handlers[msgType] = handler
	c.handlersMu.Unlock()
}

func (c *Cluster) OnNodeLeave(fn func(string)) {
	c.onNodeLeaveMu.Lock()
	c.onNodeLeave = fn
	c.onNodeLeaveMu.Unlock()
}

func (c *Cluster) SendTo(nodeName string, msgType ClusterMessageType, payload []byte) error {
	node := c.findNode(nodeName)
	if node == nil {
		return fmt.Errorf("node %q not found in cluster", nodeName)
	}

	msg := &ClusterMessage{
		Type:       msgType,
		SourceNode: c.node,
		Payload:    payload,
	}
	return c.ml.SendReliable(node, msg.Encode())
}

func (c *Cluster) Broadcast(msgType ClusterMessageType, payload []byte) {
	msg := &ClusterMessage{
		Type:       msgType,
		SourceNode: c.node,
		Payload:    payload,
	}
	encoded := msg.Encode()

	for _, member := range c.ml.Members() {
		if member.Name == c.node {
			continue
		}
		if err := c.ml.SendReliable(member, encoded); err != nil {
			c.logger.Warn("Failed to send broadcast to node", zap.String("target", member.Name), zap.Error(err))
		}
	}
}

func (c *Cluster) Request(nodeName string, msgType ClusterMessageType, payload []byte) ([]byte, error) {
	reqID := uuid.Must(uuid.NewV4())
	ch := make(chan []byte, 1)
	c.pendingRequests.Store(reqID, ch)
	defer c.pendingRequests.Delete(reqID)

	// Wrap payload in request envelope
	reqPayload := EncodeClusterRequest(reqID, payload)

	// Send as a Request-typed message wrapping the original type
	innerMsg := &ClusterMessage{
		Type:       msgType,
		SourceNode: c.node,
		Payload:    reqPayload,
	}
	innerEncoded := innerMsg.Encode()

	outerMsg := &ClusterMessage{
		Type:       ClusterMessageType_Request,
		SourceNode: c.node,
		Payload:    innerEncoded,
	}

	node := c.findNode(nodeName)
	if node == nil {
		return nil, fmt.Errorf("node %q not found in cluster", nodeName)
	}

	if err := c.ml.SendReliable(node, outerMsg.Encode()); err != nil {
		return nil, fmt.Errorf("failed to send request to %q: %w", nodeName, err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(c.config.RequestTimeout):
		return nil, fmt.Errorf("request to %q timed out", nodeName)
	}
}

func (c *Cluster) Reply(msg *ClusterMessage, response []byte) {
	if msg.RequestID == uuid.Nil {
		c.logger.Warn("Cannot reply, message has no request ID")
		return
	}

	respPayload := EncodeClusterRequest(msg.RequestID, response)
	respMsg := &ClusterMessage{
		Type:       ClusterMessageType_Response,
		SourceNode: c.node,
		Payload:    respPayload,
	}

	node := c.findNode(msg.SourceNode)
	if node == nil {
		c.logger.Warn("Cannot reply, source node not found", zap.String("source", msg.SourceNode))
		return
	}

	if err := c.ml.SendReliable(node, respMsg.Encode()); err != nil {
		c.logger.Warn("Failed to send reply", zap.String("target", msg.SourceNode), zap.Error(err))
	}
}

func (c *Cluster) findNode(name string) *memberlist.Node {
	for _, m := range c.ml.Members() {
		if m.Name == name {
			return m
		}
	}
	return nil
}

func (c *Cluster) dispatchMessage(data []byte) {
	msg, err := DecodeClusterMessage(data)
	if err != nil {
		c.logger.Warn("Failed to decode cluster message", zap.Error(err))
		return
	}

	switch msg.Type {
	case ClusterMessageType_Request:
		// Unwrap the inner message and dispatch to the handler
		innerMsg, err := DecodeClusterMessage(msg.Payload)
		if err != nil {
			c.logger.Warn("Failed to decode inner request message", zap.Error(err))
			return
		}
		// Decode request ID from inner payload, pass only actual payload to handler
		reqID, actualPayload, err := DecodeClusterRequest(innerMsg.Payload)
		if err != nil {
			c.logger.Warn("Failed to decode request envelope", zap.Error(err))
			return
		}
		innerMsg.SourceNode = msg.SourceNode
		innerMsg.RequestID = reqID
		innerMsg.Payload = actualPayload
		c.handlersMu.RLock()
		handler, ok := c.handlers[innerMsg.Type]
		c.handlersMu.RUnlock()
		if ok {
			handler(innerMsg)
		}

	case ClusterMessageType_Response:
		reqID, respPayload, err := DecodeClusterRequest(msg.Payload)
		if err != nil {
			c.logger.Warn("Failed to decode response", zap.Error(err))
			return
		}
		if ch, ok := c.pendingRequests.Load(reqID); ok {
			ch.(chan []byte) <- respPayload
		}

	default:
		c.handlersMu.RLock()
		handler, ok := c.handlers[msg.Type]
		c.handlersMu.RUnlock()
		if ok {
			handler(msg)
		}
	}
}

// clusterDelegate implements memberlist.Delegate
type clusterDelegate struct {
	cluster *Cluster
}

func (d *clusterDelegate) NotifyMsg(msg []byte) {
	// Make a copy since memberlist may reuse the buffer
	buf := make([]byte, len(msg))
	copy(buf, msg)
	d.cluster.dispatchMessage(buf)
}

func (d *clusterDelegate) NodeMeta(limit int) []byte          { return nil }
func (d *clusterDelegate) LocalState(join bool) []byte         { return nil }
func (d *clusterDelegate) MergeRemoteState(buf []byte, join bool) {}
func (d *clusterDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	return nil
}

// clusterEventDelegate implements memberlist.EventDelegate
type clusterEventDelegate struct {
	cluster *Cluster
}

func (d *clusterEventDelegate) NotifyJoin(node *memberlist.Node) {
	d.cluster.logger.Info("Node joined cluster", zap.String("node", node.Name), zap.String("addr", node.Addr.String()))
}

func (d *clusterEventDelegate) NotifyLeave(node *memberlist.Node) {
	d.cluster.logger.Info("Node left cluster", zap.String("node", node.Name))
	d.cluster.onNodeLeaveMu.RLock()
	fn := d.cluster.onNodeLeave
	d.cluster.onNodeLeaveMu.RUnlock()
	if fn != nil && node.Name != d.cluster.node {
		fn(node.Name)
	}
}

func (d *clusterEventDelegate) NotifyUpdate(node *memberlist.Node) {}

// resolveJoinAddrs resolves DNS names in join addresses for headless Service support.
func resolveJoinAddrs(addrs []string) []string {
	var resolved []string
	for _, addr := range addrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			// Try as host-only, append default port
			host = addr
			port = ""
		}

		ips, err := net.LookupHost(host)
		if err != nil {
			// Use as-is if DNS fails
			resolved = append(resolved, addr)
			continue
		}

		for _, ip := range ips {
			if port != "" {
				resolved = append(resolved, net.JoinHostPort(ip, port))
			} else {
				resolved = append(resolved, ip)
			}
		}
	}
	return resolved
}

// zapWriter adapts zap.Logger to io.Writer for memberlist log output.
type zapWriter struct {
	logger *zap.Logger
}

func newZapWriter(logger *zap.Logger) *zapWriter {
	return &zapWriter{logger: logger}
}

func (w *zapWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.logger.Debug(msg, zap.String("source", "memberlist"))
	}
	return len(p), nil
}
