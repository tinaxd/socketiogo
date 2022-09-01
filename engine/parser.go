package engine

import (
	"bytes"
	"encoding/base64"
	"errors"
)

type PacketType int
type MessageType int

const (
	PacketTypeOpen    PacketType = 0
	PacketTypeClose   PacketType = 1
	PacketTypePing    PacketType = 2
	PacketTypePong    PacketType = 3
	PacketTypeMessage PacketType = 4
	PacketTypeUpgrade PacketType = 5
	PacketTypeNoop    PacketType = 6
)

const (
	MessageTypeText MessageType = iota
	MessageTypeBinary
)

const ()

type Packet struct {
	Type     PacketType
	Data     []byte
	IsBinary bool
}

func sliceSplit(s []byte, sep byte) [][]byte {
	var res [][]byte
	var start int
	for i, c := range s {
		if c == sep {
			res = append(res, s[start:i])
			start = i + 1
		}
	}
	res = append(res, s[start:])
	return res
}

func parsePacket(packetStr []byte, isBinaryWebSocket bool) (Packet, error) {
	if isBinaryWebSocket {
		return Packet{Type: PacketTypeMessage, Data: packetStr, IsBinary: true}, nil
	}

	if len(packetStr) == 0 {
		return Packet{}, errors.New("invalid packet format")
	}

	if !isBinaryWebSocket && packetStr[0] == 'b' {
		packetDataEncoded := packetStr[1:]
		packetData, err := base64.StdEncoding.DecodeString(string(packetDataEncoded))
		if err != nil {
			return Packet{}, err
		}
		return Packet{Type: PacketTypeMessage, Data: packetData, IsBinary: true}, nil
	} else {
		packetType := PacketType(packetStr[0] - '0')
		packetData := packetStr[1:]
		return Packet{Type: packetType, Data: packetData, IsBinary: false}, nil
	}
}

func parsePayload(payloadStr []byte) ([]Packet, error) {
	packetStrs := sliceSplit(payloadStr, '\x1e')
	packets := make([]Packet, len(packetStrs))

	for i, packetStr := range packetStrs {
		packet, err := parsePacket(packetStr, false)
		if err != nil {
			return nil, err
		}
		packets[i] = packet
	}

	return packets, nil
}

func (p Packet) Encode(useWebSocket bool) []byte {
	if p.Type == PacketTypeMessage {
		var ty MessageType
		if p.IsBinary {
			ty = MessageTypeBinary
		} else {
			ty = MessageTypeText
		}
		return encodeMessagePacket(ty, p.Data, useWebSocket)
	} else {
		return append([]byte{byte(p.Type) + '0'}, p.Data...)
	}
	// log.Printf("Packet.Encode: unknown packet type: %v", p.Type)
	// return nil
}

func encodeMessagePacket(ty MessageType, data []byte, useWebSocket bool) []byte {
	if !useWebSocket {
		var (
			packetType  byte
			encodedData []byte
		)
		if ty == MessageTypeText {
			packetType = byte(PacketTypeMessage) + '0'
			encodedData = data
		} else if ty == MessageTypeBinary {
			packetType = 'b'
			encodedData = []byte(base64.StdEncoding.EncodeToString(data))
		}
		f := []byte{packetType}
		return append(f, encodedData...)
	} else {
		f := append([]byte{}, data...)
		if ty == MessageTypeText {
			f = append([]byte{'4'}, f...)
		}
		return f
	}
}

func encodePayload(packetStrs [][]byte) []byte {
	return bytes.Join(packetStrs, []byte{'\x1e'})
}

func encodeClosePacket() []byte {
	return []byte{byte(PacketTypeClose) + '0'}
}

func encodeNoopPacket() []byte {
	return []byte{byte(PacketTypeNoop) + '0'}
}
