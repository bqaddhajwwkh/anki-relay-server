package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

const (
	defaultListenAddr       = ":39461"
	defaultAuthToken        = "anki_private_pair_token_v1"
	defaultAllowedRoomID    = "anki_private_pair_room"
	defaultMaxLineBytes     = 1024 * 1024
	defaultHeartbeatSeconds = 35
	defaultSweepSeconds     = 5
	writeDeadline           = 10 * time.Second
)

type relayServer struct {
	listenAddr       string
	authToken        string
	allowedRoomID    string
	maxLineBytes     int
	heartbeatTimeout time.Duration
	sweepInterval    time.Duration

	mu    sync.Mutex
	rooms map[string]*relayRoom
	conns map[*clientConn]struct{}
}

type relayRoom struct {
	id      string
	members map[*clientConn]struct{}
}

type clientConn struct {
	conn       net.Conn
	remoteAddr string

	writeMu sync.Mutex

	mu         sync.Mutex
	lastSeenAt time.Time
	closed     bool

	deviceLabel string
	roomID      string
	joined      bool
}

func main() {
	log.SetPrefix("[anki-relay] ")
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	listenAddr := flag.String(
		"listen",
		envString("LISTEN_ADDR", defaultListenAddr),
		"listen address",
	)
	authToken := flag.String(
		"auth-token",
		envString("RELAY_AUTH_TOKEN", defaultAuthToken),
		"relay auth token",
	)
	allowedRoomID := flag.String(
		"allowed-room",
		envString("RELAY_ALLOWED_ROOM_ID", defaultAllowedRoomID),
		"allowed room id",
	)
	maxLineBytes := flag.Int(
		"max-line-bytes",
		envInt("MAX_LINE_BYTES", defaultMaxLineBytes),
		"maximum bytes per json line",
	)
	heartbeatSeconds := flag.Int(
		"heartbeat-timeout-seconds",
		envInt("HEARTBEAT_TIMEOUT_SECONDS", defaultHeartbeatSeconds),
		"heartbeat timeout seconds",
	)
	sweepSeconds := flag.Int(
		"sweep-interval-seconds",
		envInt("SWEEP_INTERVAL_SECONDS", defaultSweepSeconds),
		"stale-connection sweep interval seconds",
	)
	flag.Parse()

	server := &relayServer{
		listenAddr:       *listenAddr,
		authToken:        *authToken,
		allowedRoomID:    *allowedRoomID,
		maxLineBytes:     *maxLineBytes,
		heartbeatTimeout: time.Duration(*heartbeatSeconds) * time.Second,
		sweepInterval:    time.Duration(*sweepSeconds) * time.Second,
		rooms:            make(map[string]*relayRoom),
		conns:            make(map[*clientConn]struct{}),
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	if err := server.serve(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("server stopped with error: %v", err)
	}
}

func (s *relayServer) serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf(
		"started version=%s listen=%s allowedRoom=%q maxLineBytes=%d heartbeatTimeout=%s",
		version,
		s.listenAddr,
		s.allowedRoomID,
		s.maxLineBytes,
		s.heartbeatTimeout,
	)

	go s.sweepLoop(ctx)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
		s.shutdownAllConnections()
	}()

	var tempDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}

			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				if tempDelay == 0 {
					tempDelay = 50 * time.Millisecond
				} else {
					tempDelay *= 2
					if tempDelay > time.Second {
						tempDelay = time.Second
					}
				}
				time.Sleep(tempDelay)
				continue
			}

			return err
		}

		tempDelay = 0
		go s.handleConnection(conn)
	}
}

func (s *relayServer) handleConnection(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConn.SetNoDelay(true)
	}

	client := &clientConn{
		conn:       conn,
		remoteAddr: safeRemoteAddr(conn),
		lastSeenAt: time.Now(),
	}

	s.addConnection(client)
	log.Printf("connected remote=%s", client.remoteAddr)

	defer s.cleanupClient(client, "socket closed")

	scanner := bufio.NewScanner(conn)
	initialBufferSize := minInt(64*1024, s.maxLineBytes)
	if initialBufferSize < 1024 {
		initialBufferSize = 1024
	}
	scanner.Buffer(make([]byte, initialBufferSize), s.maxLineBytes)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		client.touch()
		s.handleIncomingLine(client, line)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("read failed remote=%s err=%v", client.remoteAddr, err)
	}
}

func (s *relayServer) handleIncomingLine(client *clientConn, line []byte) {
	var message map[string]any
	if err := json.Unmarshal(line, &message); err != nil {
		log.Printf("invalid json remote=%s err=%v", client.remoteAddr, err)
		return
	}

	messageType := strings.TrimSpace(stringValue(message["type"]))
	if messageType == "" {
		return
	}

	if strings.HasPrefix(messageType, "relay_") {
		s.handleRelayControlMessage(client, messageType, message)
		return
	}

	s.forwardPeerProtocolMessage(client, line)
}

func (s *relayServer) handleRelayControlMessage(
	client *clientConn,
	messageType string,
	message map[string]any,
) {
	switch messageType {
	case "relay_join":
		s.handleRelayJoin(client, message)
	case "relay_leave":
		s.handleRelayLeave(client)
	case "relay_ping":
		if err := client.sendJSON(map[string]any{
			"type": "relay_pong",
		}); err != nil {
			s.cleanupClient(client, "write relay_pong failed")
		}
	default:
		_ = client.sendJSON(map[string]any{
			"type":    "relay_error",
			"message": "不支持的中转控制消息",
			"fatal":   false,
		})
	}
}

func (s *relayServer) handleRelayJoin(client *clientConn, message map[string]any) {
	roomID := strings.TrimSpace(stringValue(message["roomId"]))
	authToken := strings.TrimSpace(stringValue(message["authToken"]))
	deviceLabel := strings.TrimSpace(stringValue(message["deviceLabel"]))
	if deviceLabel == "" {
		deviceLabel = client.remoteAddr
	}

	if roomID != s.allowedRoomID {
		s.sendFatalRelayErrorAndClose(client, "不允许加入该房间")
		return
	}
	if authToken != s.authToken {
		s.sendFatalRelayErrorAndClose(client, "认证失败")
		return
	}

	s.evictStaleMembersInRoom(roomID)

	var (
		peer          *clientConn
		peerLabel     string
		joinedFresh   bool
		peerReady     bool
		memberCount   int
		currentRoomID string
	)

	s.mu.Lock()

	if client.joined && client.roomID != "" && client.roomID != roomID {
		s.detachClientFromRoomLocked(client)
	}

	room := s.rooms[roomID]
	if room == nil {
		room = &relayRoom{
			id:      roomID,
			members: make(map[*clientConn]struct{}),
		}
		s.rooms[roomID] = room
	}

	_, alreadyInRoom := room.members[client]
	if !alreadyInRoom && len(room.members) >= 2 {
		s.mu.Unlock()
		s.sendFatalRelayErrorAndClose(client, "房间已满，请稍后重试")
		return
	}

	if !alreadyInRoom {
		room.members[client] = struct{}{}
		joinedFresh = true
	}

	client.deviceLabel = deviceLabel
	client.roomID = roomID
	client.joined = true
	currentRoomID = roomID

	peer = firstPeerLocked(room, client)
	if peer != nil {
		peerReady = true
		peerLabel = peer.deviceLabel
	}
	memberCount = len(room.members)

	s.mu.Unlock()

	if err := client.sendJSON(map[string]any{
		"type":            "relay_joined",
		"peerReady":       peerReady,
		"peerDeviceLabel": peerLabel,
	}); err != nil {
		s.cleanupClient(client, "write relay_joined failed")
		return
	}

	if joinedFresh && peer != nil {
		if err := peer.sendJSON(map[string]any{
			"type":            "relay_peer_joined",
			"peerDeviceLabel": deviceLabel,
		}); err != nil {
			s.cleanupClient(peer, "notify relay_peer_joined failed")
		}
	}

	log.Printf(
		"joined remote=%s room=%q label=%q peerReady=%t members=%d",
		client.remoteAddr,
		currentRoomID,
		deviceLabel,
		peerReady,
		memberCount,
	)
}

func (s *relayServer) handleRelayLeave(client *clientConn) {
	roomID, peer := s.detachClientFromRoom(client)

	if peer != nil {
		if err := peer.sendJSON(map[string]any{
			"type": "relay_peer_left",
		}); err != nil {
			s.cleanupClient(peer, "notify relay_peer_left failed")
		}
	}

	_ = client.sendJSON(map[string]any{
		"type": "relay_left",
	})

	if roomID != "" {
		log.Printf("left remote=%s room=%q", client.remoteAddr, roomID)
	}
}

func (s *relayServer) forwardPeerProtocolMessage(client *clientConn, rawLine []byte) {
	peer := s.peerOf(client)
	if peer == nil {
		_ = client.sendJSON(map[string]any{
			"type":    "relay_error",
			"message": "另一台设备尚未加入房间",
			"fatal":   false,
		})
		return
	}

	if err := peer.sendLine(rawLine); err != nil {
		s.cleanupClient(peer, "forward payload failed")
	}
}

func (s *relayServer) peerOf(client *clientConn) *clientConn {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !client.joined || client.roomID == "" {
		return nil
	}

	room := s.rooms[client.roomID]
	if room == nil {
		return nil
	}

	return firstPeerLocked(room, client)
}

func (s *relayServer) detachClientFromRoom(client *clientConn) (string, *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detachClientFromRoomLocked(client)
}

func (s *relayServer) detachClientFromRoomLocked(client *clientConn) (string, *clientConn) {
	roomID := client.roomID
	if roomID == "" {
		client.joined = false
		return "", nil
	}

	room := s.rooms[roomID]
	var peer *clientConn
	if room != nil {
		delete(room.members, client)
		peer = firstPeerLocked(room, nil)
		if len(room.members) == 0 {
			delete(s.rooms, roomID)
		}
	}

	client.joined = false
	client.roomID = ""

	return roomID, peer
}

func (s *relayServer) addConnection(client *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[client] = struct{}{}
}

func (s *relayServer) removeConnection(client *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, client)
}

func (s *relayServer) cleanupClient(client *clientConn, reason string) {
	if !client.markClosed() {
		return
	}

	_ = client.conn.Close()

	roomID, peer := s.detachClientFromRoom(client)
	s.removeConnection(client)

	if peer != nil {
		if err := peer.sendJSON(map[string]any{
			"type": "relay_peer_left",
		}); err != nil {
			s.cleanupClient(peer, "notify relay_peer_left failed")
		}
	}

	log.Printf(
		"disconnected remote=%s room=%q reason=%s",
		client.remoteAddr,
		roomID,
		reason,
	)
}

func (s *relayServer) sendFatalRelayErrorAndClose(client *clientConn, message string) {
	_ = client.sendJSON(map[string]any{
		"type":    "relay_error",
		"message": message,
		"fatal":   true,
	})
	s.cleanupClient(client, message)
}

func (s *relayServer) shutdownAllConnections() {
	s.mu.Lock()
	connections := make([]*clientConn, 0, len(s.conns))
	for client := range s.conns {
		connections = append(connections, client)
	}
	s.mu.Unlock()

	for _, client := range connections {
		s.cleanupClient(client, "server shutdown")
	}
}

func (s *relayServer) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(s.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evictAllStaleConnections()
		}
	}
}

func (s *relayServer) evictAllStaleConnections() {
	now := time.Now()

	s.mu.Lock()
	connections := make([]*clientConn, 0, len(s.conns))
	for client := range s.conns {
		connections = append(connections, client)
	}
	s.mu.Unlock()

	for _, client := range connections {
		if now.Sub(client.lastSeen()) <= s.heartbeatTimeout {
			continue
		}
		s.cleanupClient(client, "heartbeat timeout")
	}
}

func (s *relayServer) evictStaleMembersInRoom(roomID string) {
	now := time.Now()

	s.mu.Lock()
	room := s.rooms[roomID]
	if room == nil {
		s.mu.Unlock()
		return
	}

	members := make([]*clientConn, 0, len(room.members))
	for client := range room.members {
		members = append(members, client)
	}
	s.mu.Unlock()

	for _, client := range members {
		if now.Sub(client.lastSeen()) <= s.heartbeatTimeout {
			continue
		}
		s.cleanupClient(client, "join-time stale eviction")
	}
}

func firstPeerLocked(room *relayRoom, excluded *clientConn) *clientConn {
	for client := range room.members {
		if excluded != nil && client == excluded {
			continue
		}
		return client
	}
	return nil
}

func safeRemoteAddr(conn net.Conn) string {
	if conn == nil || conn.RemoteAddr() == nil {
		return "unknown"
	}
	return conn.RemoteAddr().String()
}

func (c *clientConn) touch() {
	c.mu.Lock()
	c.lastSeenAt = time.Now()
	c.mu.Unlock()
}

func (c *clientConn) lastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeenAt
}

func (c *clientConn) markClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return false
	}

	c.closed = true
	return true
}

func (c *clientConn) sendJSON(payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.sendLine(data)
}

func (c *clientConn) sendLine(line []byte) error {
	if len(line) == 0 {
		return nil
	}

	payload := append([]byte(nil), line...)
	if payload[len(payload)-1] != '\n' {
		payload = append(payload, '\n')
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	defer c.conn.SetWriteDeadline(time.Time{})

	_, err := c.conn.Write(payload)
	return err
}

func envString(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
