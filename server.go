package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

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

type ServerSocket struct {
	sid              string
	onMessageHandler OnMessageHandler
	messageQueue     chan Message
	ctx              context.Context
	canceler         context.CancelFunc
}

func (ss *ServerSocket) SetOnMessageHandler(handler OnMessageHandler) {
	ss.onMessageHandler = handler
}

func (ss *ServerSocket) Sid() string {
	return ss.sid
}

func (ss *ServerSocket) WaitForMessage() ([]Message, bool) {
	// wait for the first message
	var firstMsg Message
	select {
	case firstMsg = <-ss.messageQueue:
	case <-ss.ctx.Done():
		return nil, false
	}
	msgs := []Message{firstMsg}

	// add remain messages if exists
LOOP:
	for {
		select {
		case msg := <-ss.messageQueue:
			msgs = append(msgs, msg)
		case <-ss.ctx.Done():
			return nil, false
		default:
			break LOOP
		}
	}

	return msgs, true
}

func (ss *ServerSocket) Send(ty MessageType, data []byte) {
	log.Printf("queued data: %v", data)
	select {
	case ss.messageQueue <- Message{Type: ty, Data: data}:
	case <-ss.ctx.Done():
		log.Println("Send: context done")
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
	ctx, cancel := context.WithCancel(context.Background())
	ss := &ServerSocket{
		sid:          sid,
		messageQueue: make(chan Message, 100),
		ctx:          ctx,
		canceler:     cancel,
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

		if s.onConnectionHandler != nil {
			s.onConnectionHandler(ss)
		}
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

		if s.onConnectionHandler != nil {
			s.onConnectionHandler(ss)
		}

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
	log.Printf("messageInPolling: WaitForMessage")
	msgs, ok := ss.WaitForMessage()
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Printf("messageInPolling: WaitForMessage done")

	packets := make([][]byte, len(msgs))
	for i, msg := range msgs {
		packets[i] = encodeMessagePacket(msg.Type, msg.Data)
	}

	payload := encodePayload(packets)

	log.Printf("messageInPolling payload: %v", string(payload))

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("charset", "UTF-8")
	w.Write(payload)
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
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		s.DropConnection(ss)
		return
	}

	// log.Printf("before parse Payload, body: %v", body)

	packets, err := parsePayload(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.DropConnection(ss)
		return
	}

	// log.Printf("before validation, packets: %v", packets)

	// validation
	for _, packet := range packets {
		if packet.Type != PacketTypeMessage {
			w.WriteHeader(http.StatusBadRequest)
			s.DropConnection(ss)
			return
		}
	}

	// log.Println("before callback")

	// callbacks
	if ss.onMessageHandler != nil {
		for _, packet := range packets {
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

func (s *Server) DropConnection(ss *ServerSocket) {
	ss.canceler()

	s.socketsLock.Lock()
	delete(s.sockets, ss.sid)
	s.socketsLock.Unlock()
}
