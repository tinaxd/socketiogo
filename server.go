package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{} // use default options

const (
	TRANSPORT_POLLING   = "polling"
	TRANSPORT_WEBSOCKET = "websocket"
)

const (
	UpgradeTimeout = 5 * time.Second
)

type OnConnectionHandler func(ss *ServerSocket)

type Server struct {
	Config              ServerConfig
	sockets             map[string]*ServerSocket
	socketsLock         sync.Mutex
	onConnectionHandler OnConnectionHandler
}

func (s *Server) SetOnConnectionHandler(handler OnConnectionHandler) {
	s.onConnectionHandler = handler
}

func NewServer(config ServerConfig) *Server {
	return &Server{
		Config:  config,
		sockets: map[string]*ServerSocket{},
	}
}

type ServerConfig struct {
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
	MaxPayload   int      `json:"maxPayload"`
}

type openPacketResponse struct {
	Sid string `json:"sid"`
	ServerConfig
}

type Message struct {
	Type MessageType
	Data []byte
}

type OnMessageHandler func(data Message)

type cancelReason int

const (
	cancelReasonBadRequest cancelReason = iota
	cancelReasonDoublePolling
	cancelReasonNoPong
	cancelReasonCloseRequested
	cancelReasonPollingTimeout
)

type ServerSocket struct {
	sid string

	onMessageHandler OnMessageHandler
	messageQueue     chan Packet

	ctx          context.Context
	canceler     context.CancelFunc
	cancelReason cancelReason

	isPollingNow bool

	pingTimer    *time.Timer
	pingInterval time.Duration
	pongTimer    *time.Timer
	pongTimeout  time.Duration
	pongCtx      context.Context
	pongCanceler context.CancelFunc

	transport string
	ws        *websocket.Conn

	isUpgrading     bool
	upgradeCtx      context.Context
	upgradeCanceler context.CancelFunc
}

func (ss *ServerSocket) SetOnMessageHandler(handler OnMessageHandler) {
	ss.onMessageHandler = handler
}

func (ss *ServerSocket) Sid() string {
	return ss.sid
}

func (ss *ServerSocket) WaitForMessage(timeout time.Duration) ([]Packet, *cancelReason) {
	// wait for the first message
	var firstMsg Packet
	if timeout != 0 {
		select {
		case firstMsg = <-ss.messageQueue:
		case <-ss.ctx.Done():
			reason := ss.cancelReason
			return nil, &reason
		case <-time.After(timeout):
			reason := cancelReasonPollingTimeout
			return nil, &reason
		}
	} else {
		select {
		case firstMsg = <-ss.messageQueue:
		case <-ss.ctx.Done():
			reason := ss.cancelReason
			return nil, &reason
		}
	}
	msgs := []Packet{firstMsg}

	// add remain messages if exists
LOOP:
	for {
		select {
		case msg := <-ss.messageQueue:
			msgs = append(msgs, msg)
		case <-ss.ctx.Done():
			reason := ss.cancelReason
			return nil, &reason
		default:
			break LOOP
		}
	}

	return msgs, nil
}

func (ss *ServerSocket) Send(ty MessageType, data []byte) {
	//log.Printf("queued data: %v", data)
	packet := Packet{
		Type:     PacketTypeMessage,
		Data:     data,
		IsBinary: ty == MessageTypeBinary,
	}
	select {
	case ss.messageQueue <- packet:
	case <-ss.ctx.Done():
		//log.Println("Send: context done")
	}
}

func (ss *ServerSocket) sendPacket(p Packet) {
	select {
	case ss.messageQueue <- p:
	case <-ss.ctx.Done():
		//log.Println("sendPacket: context done")
	}
}

func (s *Server) EngineIOHandler(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	// //log.Println(v)

	eio := v.Get("EIO")
	if eio != "4" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	transport := v.Get("transport")
	if transport == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if transport != TRANSPORT_POLLING && transport != TRANSPORT_WEBSOCKET {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sid := v.Get("sid")
	if sid == "" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.handShake(w, r, transport)
		return
	} else {
		if transport == "websocket" {
			s.webSocketUpgrade(w, r, sid)
			return
		}
		if r.Method == http.MethodPost {
			s.messageOut(w, r, sid)
		} else if r.Method == http.MethodGet {
			s.messageIn(w, r, sid)
		}
	}
}

func (s *Server) handShake(w http.ResponseWriter, r *http.Request, transport string) {
	// generate sid
	var sid string
	for {
		sidUuid, err := uuid.NewRandom()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		sid = sidUuid.String()

		// ensure sid is unique
		s.socketsLock.Lock()
		_, ok := s.sockets[sid]
		s.socketsLock.Unlock()
		if !ok {
			break
		}
	}

	// register ServerSocket
	pingInterval := time.Duration(s.Config.PingInterval) * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ss := &ServerSocket{
		sid:          sid,
		messageQueue: make(chan Packet, 100),
		ctx:          ctx,
		canceler:     cancel,
		transport:    transport,
		pingInterval: pingInterval,
		pongTimeout:  time.Duration(s.Config.PingTimeout) * time.Millisecond,
	}

	// response
	if transport == TRANSPORT_POLLING {
		response := openPacketResponse{
			Sid:          sid,
			ServerConfig: s.Config,
		}
		responseText, err := json.Marshal(response)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		res := fmt.Sprintf("%d%s", PacketTypeOpen, responseText)

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("charset", "UTF-8")
		w.Write([]byte(res))

		// callback
		if s.onConnectionHandler != nil {
			s.onConnectionHandler(ss)
		}

		// ping thread
		go s.pingFunc(ss)

		s.socketsLock.Lock()
		s.sockets[sid] = ss
		s.socketsLock.Unlock()
	} else if transport == TRANSPORT_WEBSOCKET {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			//log.Print("upgrade:", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		response := openPacketResponse{
			Sid: sid,
			ServerConfig: ServerConfig{
				Upgrades:     []string{},
				PingInterval: s.Config.PingInterval,
				PingTimeout:  s.Config.PingTimeout,
				MaxPayload:   s.Config.MaxPayload,
			},
		}
		responseText, err := json.Marshal(response)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		res := fmt.Sprintf("%d%s", PacketTypeOpen, responseText)

		c.WriteMessage(websocket.TextMessage, []byte(res))

		// callback
		if s.onConnectionHandler != nil {
			s.onConnectionHandler(ss)
		}

		ss.ws = c
		s.socketsLock.Lock()
		s.sockets[sid] = ss
		s.socketsLock.Unlock()

		s.startWebSocketConnection(ss, c, sid)
	}
}

func (s *Server) startWebSocketConnection(ss *ServerSocket, c *websocket.Conn, sid string) {
	// ping thread
	go s.pingFunc(ss)

	go s.messageInWsWorker(ss)

	go func() {
		for {
			ty, b, err := c.ReadMessage()
			if err != nil {
				//log.Println("read:", err)
				s.DropConnection(ss, cancelReasonBadRequest)
				return
			}

			isBinary := ty == websocket.BinaryMessage
			ok, reason := s.messageOutProcess(b, ss, false, isBinary)
			if !ok {
				//log.Printf("websocket messageOutProcess: %v", reason)
				s.DropConnection(ss, reason)
				return
			}
		}
	}()
}

func (s *Server) webSocketUpgrade(w http.ResponseWriter, r *http.Request, sid string) {
	//log.Printf("http upgrade request")
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		//log.Print("upgrade:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	s.socketsLock.Lock()
	ss, ok := s.sockets[sid]
	if !ok {
		s.socketsLock.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		s.socketsLock.Unlock()
		return
	}

	if ss.ws != nil {
		// if websocket connection already exists, cancel upgrade
		c.Close()
		s.socketsLock.Unlock()
		return
	}

	ss.ws = c
	// do not change transport until client sends "upgrade" packet
	ss.transport = TRANSPORT_POLLING
	s.socketsLock.Unlock()

	ss.isUpgrading = true
	ss.upgradeCtx, ss.upgradeCanceler = context.WithCancel(ss.ctx)
	go func() {
		//log.Println("upgrade timeout timer started")
		// if "upgrade" packet is not received in UpgradeTimeout seconds, cancel upgrade
		select {
		case <-ss.upgradeCtx.Done():
			//log.Println("upgrade timeout timer canceled")
			return
		case <-time.After(UpgradeTimeout):
			if ss.ws != nil {
				ss.ws.Close()
				ss.ws = nil
			}
		}
		ss.isUpgrading = false
	}()

	s.startWebSocketConnection(ss, c, sid)
}

func (s *Server) messageIn(w http.ResponseWriter, r *http.Request, sid string) {
	s.socketsLock.Lock()
	ss := s.sockets[sid]
	s.socketsLock.Unlock()
	if ss == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.messageInPolling(w, r, ss)
}

func (s *Server) messageInPolling(w http.ResponseWriter, r *http.Request, ss *ServerSocket) {
	if ss.transport != TRANSPORT_POLLING {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if ss.isPollingNow {
		//log.Println("messageInPolling: already polling")
		w.WriteHeader(http.StatusInternalServerError)
		s.DropConnection(ss, cancelReasonDoublePolling)
		return
	}

	ss.isPollingNow = true

	//log.Printf("messageInPolling: WaitForMessage")
	msgs, cancelReason := ss.WaitForMessage(5 * time.Second)
	if cancelReason != nil {
		//log.Printf("WaitForMessage canceled: reason: %v", *cancelReason)
		if *cancelReason == cancelReasonDoublePolling {
			w.Write(encodeClosePacket())
			return
		} else if *cancelReason == cancelReasonCloseRequested {
			w.Write(encodeNoopPacket())
			return
		} else if *cancelReason == cancelReasonPollingTimeout {
			w.Write(encodeNoopPacket())
			return
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	//log.Printf("messageInPolling: WaitForMessage done")

	packets := make([][]byte, len(msgs))
	for i, msg := range msgs {
		packets[i] = msg.Encode(false)
	}

	payload := encodePayload(packets)

	//log.Printf("messageInPolling payload: %v", string(payload))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("charset", "UTF-8")
	w.Write(payload)

	ss.isPollingNow = false
}

func (s *Server) messageInWsWorker(ss *ServerSocket) {
	for {
		msgs, cancelReason := ss.WaitForMessage(0)
		if cancelReason != nil {
			//log.Printf("websocket WaitForMessage canceled: reason: %v", *cancelReason)
			return
		}

		type packetBin struct {
			messageType int
			data        []byte
		}
		packets := make([]packetBin, len(msgs))
		for i, msg := range msgs {
			packets[i].data = msg.Encode(true)
			if msg.IsBinary {
				packets[i].messageType = websocket.BinaryMessage
			} else {
				packets[i].messageType = websocket.TextMessage
			}
		}

		for _, packet := range packets {
			ss.ws.WriteMessage(packet.messageType, packet.data)
		}
	}
}

func (s *Server) messageOut(w http.ResponseWriter, r *http.Request, sid string) {
	s.socketsLock.Lock()
	ss := s.sockets[sid]
	s.socketsLock.Unlock()
	if ss == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.messageOutPolling(w, r, ss)
}

func (s *Server) messageOutPolling(w http.ResponseWriter, r *http.Request, ss *ServerSocket) {
	// //log.Println("messageOutPolling")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		//log.Printf("messageOutPolling: read body error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		s.DropConnection(ss, cancelReasonBadRequest)
		return
	}

	ok, reason := s.messageOutProcess(body, ss, true, false)
	if !ok {
		//log.Printf("messageOutPolling: messageOutProcess: %v", reason)
		w.WriteHeader(http.StatusBadRequest)
		s.DropConnection(ss, reason)
		return
	}

	w.Write([]byte("ok"))
}

func (s *Server) messageOutProcess(body []byte, ss *ServerSocket, usePayload bool, isBinaryWebSocket bool) (bool, cancelReason) {
	// //log.Printf("before parse Payload, body: %v", body)

	var packets []Packet
	if usePayload {
		var err error
		packets, err = parsePayload(body)
		if err != nil {
			//log.Printf("parsePayload error: %v", err)
			s.DropConnection(ss, cancelReasonBadRequest)
			return false, cancelReasonBadRequest
		}
	} else {
		packet, err := parsePacket(body, isBinaryWebSocket)
		if err != nil {
			//log.Printf("parsePacket error: %v", err)
			s.DropConnection(ss, cancelReasonBadRequest)
			return false, cancelReasonBadRequest
		}
		packets = []Packet{packet}
	}

	// //log.Printf("before validation, packets: %v", packets)

	// validation
	msgPackets := make([]Packet, 0, len(packets))
	for _, packet := range packets {
		// handle close
		if packet.Type == PacketTypeClose {
			return false, cancelReasonCloseRequested
		}

		// handle pong
		if packet.Type == PacketTypePong {
			//log.Printf("received pong")
			s.handlePong(ss)
			continue
		}

		// handle ping
		if packet.Type == PacketTypePing && bytes.Equal(packet.Data, []byte("probe")) {
			//log.Printf("received upgrade request")
			ss.sendPacket(Packet{
				Type: PacketTypePong,
				Data: []byte("probe"),
			})
			continue
		}

		// handle upgrade
		if packet.Type == PacketTypeUpgrade {
			//log.Printf("received upgrade request")
			ss.transport = TRANSPORT_WEBSOCKET
			ss.isUpgrading = false
			ss.upgradeCanceler()
			continue
		}

		if packet.Type != PacketTypeMessage {
			//log.Printf("invalid packet type: %v", packet.Type)
			return false, cancelReasonBadRequest
		}

		msgPackets = append(msgPackets, packet)
	}

	// //log.Println("before callback")

	// callbacks
	if ss.onMessageHandler != nil {
		for _, packet := range msgPackets {
			var ty MessageType
			if packet.IsBinary {
				ty = MessageTypeBinary
			} else {
				ty = MessageTypeText
			}
			ss.onMessageHandler(Message{
				Type: ty,
				Data: packet.Data,
			})
		}
	}

	// //log.Println("before response")

	// w.Write([]byte("ok"))
	return true, 0
}

func (s *Server) DropConnection(ss *ServerSocket, reason cancelReason) {
	ss.cancelReason = reason
	ss.canceler()

	if ss.ws != nil {
		if err := ss.ws.Close(); err != nil {
			//log.Printf("websocket close error: %v", err)
		}
		ss.ws = nil
	}

	s.socketsLock.Lock()
	delete(s.sockets, ss.sid)
	s.socketsLock.Unlock()
}

func (s *Server) debugDump() {
	s.socketsLock.Lock()
	defer s.socketsLock.Unlock()

	fmt.Println("=== SERVER DUMP ===")
	for sid, socket := range s.sockets {
		fmt.Printf("%v\n", sid)
		fmt.Printf("\t%v\n", socket)
	}

}
