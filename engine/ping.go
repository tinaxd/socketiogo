package engine

import (
	"context"
	"time"
)

func (s *Server) pingFunc(ss *ServerSocket) {
	//log.Printf("ping timer started")
	ss.pingTimer = time.NewTimer(ss.pingInterval)
	select {
	case <-ss.ctx.Done():
		return
	case <-ss.pingTimer.C:
		ss.pingTimer = nil
	}

	ss.sendPing()
	go s.waitForPong(ss)
}

func (ss *ServerSocket) sendPing() {
	//log.Printf("send ping")
	ss.sendPacket(Packet{
		Type: PacketTypePing,
	})
}

func (ss *ServerSocket) resetPing() {
	if ss.pingTimer != nil {
		ss.pingTimer.Reset(ss.pingInterval)
	}
}

func (s *Server) handlePong(ss *ServerSocket) {
	//log.Printf("handling pong pong")
	ss.pongCanceler()
	go s.pingFunc(ss)
}

func (s *Server) waitForPong(ss *ServerSocket) {
	ss.pongTimer = time.NewTimer(ss.pongTimeout)
	ss.pongCtx, ss.pongCanceler = context.WithCancel(ss.ctx)

	select {
	case <-ss.pongCtx.Done():
		//log.Println("waitForPong: context done")
		ss.pongTimer.Stop()
		return
	case <-ss.pongTimer.C:
		//log.Printf("pong timeout")
		s.DropConnection(ss, cancelReasonNoPong)
	}
}
