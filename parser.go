package main

import (
	"bytes"
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
	Type PacketType
	Data []byte
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

	packetType := PacketType(packetStr[0] - '0')
	packetData := packetStr[1:]
	return Packet{Type: packetType, Data: packetData}, nil
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

func encodeMessagePacket(data []byte) []byte {
	f := []byte{byte(PacketTypeMessage) + '0'}
	return append(f, data...)
}

func encodePayload(packetStrs [][]byte) []byte {
	return bytes.Join(packetStrs, []byte{'\x1e'})
}
