package socket

import "github.com/tinaxd/engineiogo/engine"

type Server struct {
	eio *engine.Server
}

func NewServer() *Server {
	return &Server{
		eio: engine.NewServer(engine.ServerConfig{
			Upgrades:     []string{engine.TRANSPORT_WEBSOCKET},
			PingInterval: 300,
			PingTimeout:  200,
			MaxPayload:   1e6,
		}),
	}
}
