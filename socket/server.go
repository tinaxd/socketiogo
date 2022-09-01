package socket

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/tinaxd/socketiogo/engine"
)

type OnConnectionHandler func(ss *ServerSocket, handshake []byte)

type Namespace struct {
	sockets             map[string]*ServerSocket // socket.io sid -> socket.io
	socketsLock         sync.Mutex
	name                string // namespace name
	onConnectionHandler OnConnectionHandler
}

func NewNamespace(nsp string) *Namespace {
	return &Namespace{
		sockets: make(map[string]*ServerSocket),
		name:    nsp,
	}
}

func (ns *Namespace) Name() string {
	return ns.name
}

func (ns *Namespace) SetOnConnectionHandler(f OnConnectionHandler) {
	ns.onConnectionHandler = f
}

type Server struct {
	eio *engine.Server

	esidMap     map[string]*ServerSocket // engine.io sid (esid) -> socket.io
	esidMapLock sync.Mutex
	sockets     map[string]*ServerSocket // socket.io sid (sid) -> socket.io
	socketsLock sync.Mutex

	namespaces     map[string]*Namespace // nsp -> namespace
	namespacesLock sync.Mutex
}

func (s *Server) Namespace(nsp string) *Namespace {
	s.namespacesLock.Lock()
	defer s.namespacesLock.Unlock()

	ns, ok := s.namespaces[nsp]
	if !ok {
		ns = NewNamespace(nsp)
		s.namespaces[nsp] = ns
	}
	return ns
}

type AckInfo struct {
	ackId int
	ss    *ServerSocket
	s     *Server
}

type Message struct {
	Args      []interface{}
	ack       *AckInfo
	namespace string
}

func (m *Message) HasAck() bool {
	return m.ack != nil
}

func (m *Message) Ack(jsonSerializable interface{}) error {
	if m.ack == nil {
		return errors.New("no ack")
	}

	ackPacket := NewPacket(PacketTypeAck, 0, m.namespace, jsonSerializable, &m.ack.ackId)
	return m.ack.s.send(m.ack.ss, ackPacket)
}

type EventCallback func(msg *Message)

type EventHandler struct {
	callback EventCallback
}

func CreateEventHandler(c EventCallback) EventHandler {
	return EventHandler{
		callback: c,
	}
}

type ServerSocket struct {
	s *Server

	sid string

	namespace string
	joined    bool

	engineSocket *engine.ServerSocket

	eventHandlers     map[string]EventHandler // event name -> EventHandler
	eventHandlersLock sync.Mutex
}

func (ss *ServerSocket) Sid() string {
	return ss.sid
}

func (ss *ServerSocket) SetEventHandler(event string, h EventHandler) {
	ss.eventHandlersLock.Lock()
	ss.eventHandlers[event] = h
	ss.eventHandlersLock.Unlock()
}

func (ss *ServerSocket) Emit(event string, args []interface{}) error {
	payload := []interface{}{event}
	payload = append(payload, args...)
	return ss.send(NewPacket(PacketTypeEvent, 0, ss.namespace, payload, nil))
}

type AckCallback func(args interface{})

func (ss *ServerSocket) EmitWithAck(event string, args []interface{}, cb AckCallback) error {
	n, err := rand.Int(rand.Reader, big.NewInt(1e6))
	if err != nil {
		return err
	}
	ackId := int(n.Int64())
	return ss.send(NewPacket(PacketTypeEvent, 0, ss.namespace, []interface{}{event, args}, &ackId))
}

func NewServer() *Server {
	eio := engine.NewServer(engine.ServerConfig{
		Upgrades:     []string{engine.TRANSPORT_WEBSOCKET},
		PingInterval: 300,
		PingTimeout:  200,
		MaxPayload:   1e6,
	})

	s := &Server{
		eio:        eio,
		esidMap:    make(map[string]*ServerSocket),
		sockets:    make(map[string]*ServerSocket),
		namespaces: make(map[string]*Namespace),
	}

	eio.SetOnConnectionHandler(func(ess *engine.ServerSocket) {
		ss, err := s.addServerSocket(ess)
		if err != nil {
			log.Println(err)
			s.DropConnection(ss)
			return
		}
		ess.SetOnMessageHandler(func(data engine.Message) {
			s.messageHandler(ss, data)
		})
	})

	return s
}

func (s *Server) addServerSocket(ess *engine.ServerSocket) (*ServerSocket, error) {
	// generate sid
	var sid string
	for {
		sidUuid, err := uuid.NewRandom()
		if err != nil {
			return nil, err
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

	ss := &ServerSocket{
		s:            s,
		sid:          sid,
		engineSocket: ess,
	}

	s.socketsLock.Lock()
	s.sockets[sid] = ss
	s.socketsLock.Unlock()

	s.esidMapLock.Lock()
	s.esidMap[ess.Sid()] = ss
	s.esidMapLock.Unlock()

	return ss, nil
}

func (s *Server) messageHandler(ss *ServerSocket, msg engine.Message) {
	p, err := parsePacket(msg.Data, false)
	if err != nil {
		log.Println(err)
		s.DropConnection(ss)
	}

	switch p.Type {
	case PacketTypeConnect:
		s.handleConnect(ss, p)
	case PacketTypeDisconnect:
		s.handleDisconnect(ss, p)
	case PacketTypeEvent:
		s.handleEvent(ss, p)
	}
}

func (s *Server) handleConnect(ss *ServerSocket, p Packet) {
	nsp := p.Namespace
	s.connectNamespace(ss, nsp)

	// send connect packet
	s.send(ss, createConnectNamespace(nsp, ss.sid))

	// callback
	s.namespacesLock.Lock()
	defer s.namespacesLock.Unlock()
	ns, ok := s.namespaces[nsp]
	if !ok {
		log.Printf("handleConnect: namespace %s not found", nsp)
		return
	}
	if ns.onConnectionHandler != nil {
		ns.onConnectionHandler(ss, p.Payload.([]byte))
	}
}

func (s *Server) handleDisconnect(ss *ServerSocket, p Packet) {
	nsp := p.Namespace
	s.disconnectFromNamespace(ss, nsp)
}

func (s *Server) handleEvent(ss *ServerSocket, p Packet) {
	if !p.payloadIsEncoded {
		panic("payload is already decoded (unreachable)")
	}

	var decoded []interface{}
	if err := json.Unmarshal(p.Payload.([]byte), &decoded); err != nil {
		log.Println(err)
		s.DropConnection(ss)
		return
	}

	if len(decoded) < 1 {
		log.Println("handleEvent: invalid payload")
		s.DropConnection(ss)
		return
	}

	event := decoded[0].(string)
	args := decoded[1:]

	var ack *AckInfo = nil
	if p.AckId != nil {
		ack = &AckInfo{
			ackId: *p.AckId,
			ss:    ss,
			s:     s,
		}
	}

	var (
		h  EventHandler
		ok bool
	)
	func() {
		ss.eventHandlersLock.Lock()
		defer ss.eventHandlersLock.Unlock()
		h, ok = ss.eventHandlers[event]
	}()

	if !ok {
		return
	}

	h.callback(&Message{
		Args:      args,
		ack:       ack,
		namespace: p.Namespace,
	})
}

func (s *Server) send(ss *ServerSocket, p Packet) error {
	enc, err := p.encode()
	if err != nil {
		return err
	}
	ss.engineSocket.Send(engine.MessageTypeText, enc)
	return nil
}

func (ss *ServerSocket) send(p Packet) error {
	return ss.s.send(ss, p)
}

func (s *Server) connectNamespace(ss *ServerSocket, nsp string) {
	if ss.joined {
		s.disconnectFromNamespace(ss, ss.namespace)
	}

	s.namespacesLock.Lock()
	ns, ok := s.namespaces[nsp]
	if !ok {
		ns = NewNamespace(nsp)
		s.namespaces[nsp] = ns
	}
	s.namespacesLock.Unlock()

	ns.socketsLock.Lock()
	ns.sockets[ss.sid] = ss
	ns.socketsLock.Unlock()

	ss.namespace = nsp
	ss.joined = true
}

func (s *Server) disconnectFromNamespace(ss *ServerSocket, nsp string) {
	s.namespacesLock.Lock()
	ns, ok := s.namespaces[nsp]
	if !ok {
		log.Printf("disconnectFromNamespace: namespace %s not found", nsp)
		return
	}
	s.namespacesLock.Unlock()

	ns.socketsLock.Lock()
	delete(ns.sockets, ss.sid)
	ns.socketsLock.Unlock()

	ss.joined = false
	ss.namespace = ""
}

func (s *Server) DropConnection(ss *ServerSocket) {
	delete(s.sockets, ss.sid)
	delete(s.esidMap, ss.engineSocket.Sid())
	s.eio.CloseConnection(ss.engineSocket)
}

func (s *Server) HandleFunc(w http.ResponseWriter, r *http.Request) {
	s.eio.EngineIOHandler(w, r)
}
