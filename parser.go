package main

import (
	"bytes"
	"errors"
	"log"
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

func parsePacket(packetStr []byte) (Packet, error) {
	if len(packetStr) == 0 {
		return Packet{}, errors.New("invalid packet format")
	}

	if packetStr[0] == 'b' {
		packetData := packetStr[1:]
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
		packet, err := parsePacket(packetStr)
		if err != nil {
			return nil, err
		}
		packets[i] = packet
	}

	return packets, nil
}

func (p Packet) Encode() []byte {
	if p.Type == PacketTypeMessage {
		var ty MessageType
		if p.IsBinary {
			ty = MessageTypeBinary
		} else {
			ty = MessageTypeText
		}
		return encodeMessagePacket(ty, p.Data)
	} else {
		return []byte{byte(p.Type) + '0'}
	}
	log.Printf("Packet.Encode: unknown packet type: %v", p.Type)
	return nil
}

func encodeMessagePacket(ty MessageType, data []byte) []byte {
	var packetType byte
	if ty == MessageTypeText {
		packetType = byte(PacketTypeMessage) + '0'
	} else if ty == MessageTypeBinary {
		packetType = 'b'
	}
	f := []byte{packetType}
	return append(f, data...)
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
