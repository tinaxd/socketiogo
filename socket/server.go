package socket

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"

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

	nextAckId int32
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

type partialMessage struct {
	Args         binJsonValue
	Ack          *AckInfo
	Namespace    string
	Event        string
	Placeholders map[int]binJsonValue // placeholder num -> placeholder position
}

type ServerSocket struct {
	s *Server

	sid string

	namespace string
	joined    bool

	engineSocket *engine.ServerSocket

	eventHandlers     map[string]EventHandler // event name -> EventHandler
	eventHandlersLock sync.Mutex

	pm               partialMessage
	requiredBinaries int
	currentBinary    int
	binaryLock       sync.Mutex
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

func (s *Server) generateAckId() int {
	n := atomic.AddInt32(&s.nextAckId, 1)
	return int(n)
}

func (ss *ServerSocket) EmitWithAck(event string, args []interface{}, cb AckCallback) error {
	n := ss.s.generateAckId()
	ackId := int(n)
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
		nextAckId:  0,
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
		s:                s,
		sid:              sid,
		engineSocket:     ess,
		eventHandlers:    make(map[string]EventHandler),
		namespace:        "/",
		requiredBinaries: 0,
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
	log.Printf("new engine message: %v:%v", msg.Type, msg.Data)
	finish, err := func() (bool, error) {
		ss.binaryLock.Lock()
		sb := ss.requiredBinaries
		ss.binaryLock.Unlock()
		if sb == 0 {
			return false, nil
		}
		if msg.Type == engine.MessageTypeBinary {
			s.handleBinary(ss, msg.Data)
			return true, nil
		} else {
			log.Printf("expected binary message, got %v", msg)
			return false, errors.New("expected binary message")
		}
	}()

	if err != nil {
		s.DropConnection(ss)
		return
	}

	if finish {
		return
	}

	log.Println("handle non-binary")

	p, err := parsePacket(msg.Data, false)
	if err != nil {
		log.Println(err)
		s.DropConnection(ss)
		return
	}
	log.Printf("Packet: %v", p)

	switch p.Type {
	case PacketTypeConnect:
		s.handleConnect(ss, p)
	case PacketTypeDisconnect:
		s.handleDisconnect(ss, p)
	case PacketTypeEvent:
		s.handleEvent(ss, p, false)
	case PacketTypeBinaryEvent:
		s.handleEvent(ss, p, true)
	}
}

func (s *Server) handleConnect(ss *ServerSocket, p Packet) {
	nsp := p.Namespace
	success := s.connectNamespace(ss, nsp)
	log.Printf("nsp: %v, success: %v", nsp, success)

	if !success {
		ss.send(createConnectNamespaceFailure(nsp, "Invalid namespace"))
		return
	}

	// send connect packet
	ss.send(createConnectNamespace(nsp, ss.sid))

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

func (s *Server) handleBinary(ss *ServerSocket, bin []byte) {
	log.Println("handle binary")
	ss.binaryLock.Lock()
	defer ss.binaryLock.Unlock()
	ss.currentBinary++
	gotBinaries := ss.currentBinary
	placeholderNum := gotBinaries - 1

	// assign binary to partial message
	toBeReplaced, ok := ss.pm.Placeholders[placeholderNum]
	if !ok {
		log.Println("got unexpected binary")
		s.DropConnection(ss)
		return
	}
	toBeReplaced.replacePlaceholder(placeholderNum, bin)

	if gotBinaries == ss.requiredBinaries {
		ss.requiredBinaries = 0

		// got all binaries, complete the message
		obj := convertToJson(ss.pm.Args)
		arrayObj, ok := obj.([]interface{})
		if !ok {
			log.Println("got unexpected message")
			s.DropConnection(ss)
			return
		}

		// callback
		func() {
			ss.eventHandlersLock.Lock()
			defer ss.eventHandlersLock.Unlock()
			h, ok := ss.eventHandlers[ss.pm.Event]
			if ok {
				h.callback(&Message{
					Args:      arrayObj,
					namespace: ss.pm.Namespace,
					ack:       ss.pm.Ack,
				})
			}
		}()

		ss.pm.Ack = nil
		ss.pm.Args = nil
		ss.pm.Namespace = ""
		ss.pm.Event = ""
	}
}

func (s *Server) handleEvent(ss *ServerSocket, p Packet, hasBinary bool) {
	if hasBinary {
		ss.binaryLock.Lock()
		defer ss.binaryLock.Unlock()
		ss.requiredBinaries = p.NBinaryAttachments
		ss.currentBinary = 0
	}

	if !p.payloadIsEncoded {
		panic("payload is already decoded (unreachable)")
	}

	var decodedI interface{}
	if err := json.Unmarshal(p.Payload.([]byte), &decodedI); err != nil {
		log.Printf("handleEvent: %v", err)
		s.DropConnection(ss)
		return
	}

	decoded, ok := decodedI.([]interface{})
	if !ok {
		log.Println("handleEvent: invalid payload (not an array)")
		s.DropConnection(ss)
		return
	}
	if len(decoded) < 1 {
		log.Println("handleEvent: invalid payload (empty array)")
		s.DropConnection(ss)
		return
	}

	event, ok := decoded[0].(string)
	if !ok {
		log.Println("handleEvent: invalid payload (event name not a string)")
		s.DropConnection(ss)
		return
	}

	args := decoded[1:]

	var ack *AckInfo = nil
	if p.AckId != nil {
		ack = &AckInfo{
			ackId: *p.AckId,
			ss:    ss,
			s:     s,
		}
	}

	log.Printf("event=%s args=%v", event, args)

	var (
		h EventHandler
	)
	ss.eventHandlersLock.Lock()
	h, ok = ss.eventHandlers[event]
	ss.eventHandlersLock.Unlock()

	if !ok {
		return
	}

	if !hasBinary {
		h.callback(&Message{
			Args:      args,
			ack:       ack,
			namespace: p.Namespace,
		})
	} else {
		// search for binary placeholders
		bArgs, placeholders := convertToBinJson(args)
		count := len(placeholders)

		if count != p.NBinaryAttachments {
			// invalid format
			log.Printf("handleEvent: invalid binary placeholders count: %v", count)
			s.DropConnection(ss)
			return
		}

		// store in partialMessage
		ss.pm.Args = bArgs
		ss.pm.Namespace = p.Namespace
		ss.pm.Ack = ack
		ss.pm.Event = event
		ss.pm.Placeholders = placeholders
	}
}

func (s *Server) send(ss *ServerSocket, p Packet) error {
	enc, err := p.encode()
	if err != nil {
		return err
	}
	ss.engineSocket.Send(engine.MessageTypeText, enc)
	return nil
}

func (s *Server) sendBinary(ss *ServerSocket, data []byte) error {
	ss.engineSocket.Send(engine.MessageTypeBinary, data)
	return nil
}

func (ss *ServerSocket) send(p Packet) error {
	return ss.s.send(ss, p)
}

func (ss *ServerSocket) sendBinary(data []byte) error {
	return ss.s.sendBinary(ss, data)
}

func (s *Server) connectNamespace(ss *ServerSocket, nsp string) bool {
	s.namespacesLock.Lock()
	ns, ok := s.namespaces[nsp]
	s.namespacesLock.Unlock()

	if !ok {
		return false
	}

	log.Printf("connecting to " + nsp)
	if ss.joined {
		s.disconnectFromNamespace(ss, ss.namespace)
	}

	ns.socketsLock.Lock()
	ns.sockets[ss.sid] = ss
	ns.socketsLock.Unlock()

	ss.namespace = nsp
	ss.joined = true
	return true
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
	ss.namespace = "/"
}

func (s *Server) DropConnection(ss *ServerSocket) {
	delete(s.sockets, ss.sid)
	delete(s.esidMap, ss.engineSocket.Sid())
	s.eio.CloseConnection(ss.engineSocket)
}

func (s *Server) HandleFunc(w http.ResponseWriter, r *http.Request) {
	s.eio.EngineIOHandler(w, r)
}
