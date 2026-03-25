package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

type Config struct {
	ListenAddr    string
	AuthToken     string
	AllowedRoomID string
	MaxLineBytes  int
}

type RelayServer struct {
	config Config
	logger *log.Logger

	mu    sync.Mutex
	rooms map[string]*RelayRoom
}

type RelayRoom struct {
	id      string
	clients []*ClientConn
}

type ClientConn struct {
	server      *RelayServer
	conn        net.Conn
	remoteAddr  string
	roomID      string
	deviceLabel string
	joined      bool

	writeMu sync.Mutex
}

func main() {
	cfg, printVersion := parseConfig()
	if printVersion {
		fmt.Println(version)
		return
	}

	logger := log.New(os.Stdout, "[anki-relay] ", log.LstdFlags|log.Lmicroseconds)

	server := &RelayServer{
		config: cfg,
		logger: logger,
		rooms:  map[string]*RelayRoom{},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx); err != nil {
		logger.Fatalf("server stopped with error: %v", err)
	}
}

func parseConfig() (Config, bool) {
	defaultListenAddr := envString("LISTEN_ADDR", ":39461")
	defaultAuthToken := envString("RELAY_AUTH_TOKEN", "anki_private_pair_token_v1")
	defaultAllowedRoomID := envString("RELAY_ALLOWED_ROOM_ID", "anki_private_pair_room")
	defaultMaxLineBytes := envInt("MAX_LINE_BYTES", 1024*1024)

	listenAddr := flag.String("listen-addr", defaultListenAddr, "TCP listen address")
	authToken := flag.String("auth-token", defaultAuthToken, "relay auth token")
	allowedRoomID := flag.String("allowed-room-id", defaultAllowedRoomID, "allowed room id; empty means allow any room")
	maxLineBytes := flag.Int("max-line-bytes", defaultMaxLineBytes, "maximum bytes allowed for one JSON line")
	printVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	return Config{
		ListenAddr:    strings.TrimSpace(*listenAddr),
		AuthToken:     strings.TrimSpace(*authToken),
		AllowedRoomID: strings.TrimSpace(*allowedRoomID),
		MaxLineBytes:  *maxLineBytes,
	}, *printVersion
}

func envString(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func (s *RelayServer) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen failed on %s: %w", s.config.ListenAddr, err)
	}
	defer listener.Close()

	s.logger.Printf(
		"started version=%s listen=%s allowedRoom=%q maxLineBytes=%d",
		version,
		s.config.ListenAddr,
		s.config.AllowedRoomID,
		s.config.MaxLineBytes,
	)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.logger.Printf("shutting down")
				return nil
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				s.logger.Printf("temporary accept error: %v", err)
				time.Sleep(300 * time.Millisecond)
				continue
			}

			return fmt.Errorf("accept failed: %w", err)
		}

		go s.handleConnection(conn)
	}
}

func (s *RelayServer) handleConnection(conn net.Conn) {
	enableTCPHints(conn)

	client := &ClientConn{
		server:     s,
		conn:       conn,
		remoteAddr: conn.RemoteAddr().String(),
	}

	s.logger.Printf("accepted remote=%s", client.remoteAddr)

	defer func() {
		s.handleDisconnect(client)
		_ = client.conn.Close()
		s.logger.Printf("closed remote=%s room=%q joined=%v", client.remoteAddr, client.roomID, client.joined)
	}()

	reader := bufio.NewReaderSize(conn, 128*1024)

	for {
		line, err := readLineWithLimit(reader, s.config.MaxLineBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}

			s.logger.Printf("read failed remote=%s err=%v", client.remoteAddr, err)
			_ = client.sendJSON(map[string]any{
				"type":    "relay_error",
				"message": fmt.Sprintf("读取消息失败: %v", err),
			})
			return
		}

		if strings.TrimSpace(line) == "" {
			continue
		}

		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			s.logger.Printf("invalid json remote=%s err=%v", client.remoteAddr, err)
			_ = client.sendJSON(map[string]any{
				"type":    "relay_error",
				"message": "JSON 格式错误",
			})
			return
		}

		messageType := stringField(message, "type")
		if messageType == "" {
			_ = client.sendJSON(map[string]any{
				"type":    "relay_error",
				"message": "缺少 type 字段",
			})
			return
		}

		if !client.joined {
			if messageType != "relay_join" {
				_ = client.sendJSON(map[string]any{
					"type":    "relay_error",
					"message": "连接建立后必须先发送 relay_join",
				})
				return
			}

			if err := s.handleJoin(client, message); err != nil {
				s.logger.Printf("join failed remote=%s err=%v", client.remoteAddr, err)
				_ = client.sendJSON(map[string]any{
					"type":    "relay_error",
					"message": err.Error(),
				})
				return
			}

			continue
		}

		if strings.HasPrefix(messageType, "relay_") {
			_ = client.sendJSON(map[string]any{
				"type":    "relay_error",
				"message": "不支持客户端主动发送该 relay 控制消息",
			})
			continue
		}

		if err := s.forwardToPeer(client, []byte(line)); err != nil {
			s.logger.Printf("forward failed remote=%s room=%s type=%s err=%v", client.remoteAddr, client.roomID, messageType, err)
			_ = client.sendJSON(map[string]any{
				"type":    "relay_error",
				"message": err.Error(),
			})
		}
	}
}

func (s *RelayServer) handleJoin(client *ClientConn, message map[string]any) error {
	roomID := stringField(message, "roomId")
	authToken := stringField(message, "authToken")
	deviceLabel := stringField(message, "deviceLabel")

	if roomID == "" {
		return errors.New("roomId 不能为空")
	}
	if authToken == "" {
		return errors.New("authToken 不能为空")
	}
	if authToken != s.config.AuthToken {
		return errors.New("authToken 不正确")
	}
	if s.config.AllowedRoomID != "" && roomID != s.config.AllowedRoomID {
		return fmt.Errorf("roomId 不允许: %s", roomID)
	}
	if deviceLabel == "" {
		deviceLabel = "未命名设备"
	}

	var peer *ClientConn

	s.mu.Lock()

	room := s.rooms[roomID]
	if room == nil {
		room = &RelayRoom{id: roomID}
		s.rooms[roomID] = room
	}

	if len(room.clients) >= 2 {
		s.mu.Unlock()
		return fmt.Errorf("房间 %s 已满，只允许两台设备同时在线", roomID)
	}

	client.joined = true
	client.roomID = roomID
	client.deviceLabel = deviceLabel
	room.clients = append(room.clients, client)

	if len(room.clients) == 2 {
		peer = room.peerOf(client)
	}

	s.mu.Unlock()

	joinReply := map[string]any{
		"type":      "relay_joined",
		"peerReady": peer != nil,
	}
	if peer != nil && peer.deviceLabel != "" {
		joinReply["peerDeviceLabel"] = peer.deviceLabel
	}

	if err := client.sendJSON(joinReply); err != nil {
		return fmt.Errorf("发送 relay_joined 失败: %w", err)
	}

	if peer == nil {
		s.logger.Printf("joined waiting room=%s remote=%s device=%s", roomID, client.remoteAddr, client.deviceLabel)
		return nil
	}

	if err := peer.sendJSON(map[string]any{
		"type":            "relay_peer_joined",
		"peerDeviceLabel": client.deviceLabel,
	}); err != nil {
		s.logger.Printf("notify peer joined failed room=%s err=%v", roomID, err)
	}

	s.logger.Printf(
		"paired room=%s left=%s(%s) right=%s(%s)",
		roomID,
		peer.remoteAddr,
		peer.deviceLabel,
		client.remoteAddr,
		client.deviceLabel,
	)

	return nil
}

func (s *RelayServer) forwardToPeer(sender *ClientConn, rawLine []byte) error {
	s.mu.Lock()
	room := s.rooms[sender.roomID]
	var peer *ClientConn
	if room != nil {
		peer = room.peerOf(sender)
	}
	s.mu.Unlock()

	if peer == nil {
		return errors.New("另一台设备尚未上线")
	}

	if err := peer.writeLine(rawLine); err != nil {
		return fmt.Errorf("转发到对端失败: %w", err)
	}

	return nil
}

func (s *RelayServer) handleDisconnect(client *ClientConn) {
	if !client.joined || client.roomID == "" {
		return
	}

	var peer *ClientConn
	var roomBecameEmpty bool

	s.mu.Lock()

	room := s.rooms[client.roomID]
	if room != nil {
		peer = room.remove(client)
		if len(room.clients) == 0 {
			delete(s.rooms, client.roomID)
			roomBecameEmpty = true
		}
	}

	s.mu.Unlock()

	if peer != nil {
		if err := peer.sendJSON(map[string]any{
			"type": "relay_peer_left",
		}); err != nil {
			s.logger.Printf("notify peer left failed room=%s err=%v", client.roomID, err)
		}
	}

	if roomBecameEmpty {
		s.logger.Printf("room removed room=%s", client.roomID)
	}
}

func (r *RelayRoom) peerOf(current *ClientConn) *ClientConn {
	for _, item := range r.clients {
		if item != current {
			return item
		}
	}
	return nil
}

func (r *RelayRoom) remove(target *ClientConn) *ClientConn {
	var nextClients []*ClientConn
	var peer *ClientConn

	for _, item := range r.clients {
		if item == target {
			continue
		}
		nextClients = append(nextClients, item)
		peer = item
	}

	r.clients = nextClients
	return peer
}

func (c *ClientConn) sendJSON(message map[string]any) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return c.writeLine(raw)
}

func (c *ClientConn) writeLine(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	payload := append([]byte(nil), raw...)
	if payload[len(payload)-1] != '\n' {
		payload = append(payload, '\n')
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.conn.Write(payload)
	return err
}

func stringField(message map[string]any, key string) string {
	value, ok := message[key]
	if !ok || value == nil {
		return ""
	}

	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func readLineWithLimit(reader *bufio.Reader, maxLineBytes int) (string, error) {
	var buffer bytes.Buffer

	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			if buffer.Len()+len(part) > maxLineBytes {
				return "", fmt.Errorf("单条消息超过限制 %d 字节", maxLineBytes)
			}
			buffer.Write(part)
		}

		if err == nil {
			return strings.TrimRight(buffer.String(), "\r\n"), nil
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		if errors.Is(err, io.EOF) {
			if buffer.Len() == 0 {
				return "", io.EOF
			}
			return strings.TrimRight(buffer.String(), "\r\n"), nil
		}

		return "", err
	}
}

func enableTCPHints(conn net.Conn) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}

	_ = tcpConn.SetKeepAlive(true)
	_ = tcpConn.SetKeepAlivePeriod(2 * time.Minute)
	_ = tcpConn.SetNoDelay(true)
}
