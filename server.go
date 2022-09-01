package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
}

func (ss *ServerSocket) SetOnMessageHandler(handler OnMessageHandler) {
	ss.onMessageHandler = handler
}

func (ss *ServerSocket) Sid() string {
	return ss.sid
}

func (ss *ServerSocket) WaitForMessage() ([]Packet, *cancelReason) {
	// wait for the first message
	var firstMsg Packet
	select {
	case firstMsg = <-ss.messageQueue:
	case <-ss.ctx.Done():
		reason := ss.cancelReason
		return nil, &reason
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
	log.Printf("queued data: %v", data)
	packet := Packet{
		Type:     PacketTypeMessage,
		Data:     data,
		IsBinary: ty == MessageTypeBinary,
	}
	select {
	case ss.messageQueue <- packet:
	case <-ss.ctx.Done():
		log.Println("Send: context done")
	}
}

func (ss *ServerSocket) sendPacket(p Packet) {
	select {
	case ss.messageQueue <- p:
	case <-ss.ctx.Done():
		log.Println("sendPacket: context done")
	}
}

func (s *Server) engineIOHandler(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	// log.Println(v)

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
		if r.Method == http.MethodPost {
			s.messageOut(w, r, transport, sid)
		} else if r.Method == http.MethodGet {
			s.messageIn(w, r, transport, sid)
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
	s.socketsLock.Lock()
	s.sockets[sid] = ss
	s.socketsLock.Unlock()

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
	} else if transport == TRANSPORT_WEBSOCKET {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer c.Close()

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

		// ping thread
		go s.pingFunc(ss)

		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				break
			}
		}
	}
}

func (s *Server) messageIn(w http.ResponseWriter, r *http.Request, transport string, sid string) {
	s.socketsLock.Lock()
	ss := s.sockets[sid]
	s.socketsLock.Unlock()
	if ss == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if transport == TRANSPORT_POLLING {
		s.messageInPolling(w, r, ss)
	} else if transport == TRANSPORT_WEBSOCKET {
		log.Println("messageIn websocket")
	}
}

func (s *Server) messageInPolling(w http.ResponseWriter, r *http.Request, ss *ServerSocket) {
	if ss.isPollingNow {
		log.Println("messageInPolling: already polling")
		w.WriteHeader(http.StatusInternalServerError)
		s.DropConnection(ss, cancelReasonDoublePolling)
		return
	}

	ss.isPollingNow = true

	log.Printf("messageInPolling: WaitForMessage")
	msgs, cancelReason := ss.WaitForMessage()
	if cancelReason != nil {
		log.Printf("WaitForMessage canceled: reason: %v", *cancelReason)
		if *cancelReason == cancelReasonDoublePolling {
			w.Write(encodeClosePacket())
			return
		} else if *cancelReason == cancelReasonCloseRequested {
			w.Write(encodeNoopPacket())
			return
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	log.Printf("messageInPolling: WaitForMessage done")

	packets := make([][]byte, len(msgs))
	for i, msg := range msgs {
		packets[i] = msg.Encode()
	}

	payload := encodePayload(packets)

	log.Printf("messageInPolling payload: %v", string(payload))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("charset", "UTF-8")
	w.Write(payload)

	ss.isPollingNow = false
}

func (s *Server) messageOut(w http.ResponseWriter, r *http.Request, transport string, sid string) {
	s.socketsLock.Lock()
	ss := s.sockets[sid]
	s.socketsLock.Unlock()
	if ss == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if transport == TRANSPORT_POLLING {
		s.messageOutPolling(w, r, ss)
	} else if transport == TRANSPORT_WEBSOCKET {
		log.Println("messageOut websocket")
	}
}

func (s *Server) messageOutPolling(w http.ResponseWriter, r *http.Request, ss *ServerSocket) {
	// log.Println("messageOutPolling")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("messageOutPolling: read body error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		s.DropConnection(ss, cancelReasonBadRequest)
		return
	}

	// log.Printf("before parse Payload, body: %v", body)

	packets, err := parsePayload(body)
	if err != nil {
		log.Printf("parsePayload error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		s.DropConnection(ss, cancelReasonBadRequest)
		return
	}

	// log.Printf("before validation, packets: %v", packets)

	// validation
	msgPackets := make([]Packet, 0, len(packets))
	for _, packet := range packets {
		// handle close
		if packet.Type == PacketTypeClose {
			s.DropConnection(ss, cancelReasonCloseRequested)
			return
		}

		// handle pong
		if packet.Type == PacketTypePong {
			log.Printf("received pong")
			s.handlePong(ss)
			continue
		}

		if packet.Type != PacketTypeMessage {
			log.Printf("invalid packet type: %v", packet.Type)
			w.WriteHeader(http.StatusBadRequest)
			s.DropConnection(ss, cancelReasonBadRequest)
			return
		}

		msgPackets = append(msgPackets, packet)
	}

	// log.Println("before callback")

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

	// log.Println("before response")

	w.Write([]byte("ok"))
}

func (s *Server) DropConnection(ss *ServerSocket, reason cancelReason) {
	ss.cancelReason = reason
	ss.canceler()

	s.socketsLock.Lock()
	delete(s.sockets, ss.sid)
	s.socketsLock.Unlock()
}
