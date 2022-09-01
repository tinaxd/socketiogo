package main

func (ss *ServerSocket) PingFunc() {
	select {
	case <-ss.ctx.Done():
		return
	case <-ss.pingTimer.C:
	}

	ss.sendPing()
}

func (ss *ServerSocket) sendPing() {
	ss.sendPacket(Packet{
		Type: PacketTypePing,
	})
}
