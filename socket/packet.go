package socket

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

type PacketType int

const (
	PacketTypeConnect      PacketType = 0
	PacketTypeDisconnect   PacketType = 1
	PacketTypeEvent        PacketType = 2
	PacketTypeAck          PacketType = 3
	PacketTypeConnectError PacketType = 4
	PacketTypeBinaryEvent  PacketType = 5
	PacketTypeBinaryAck    PacketType = 6
)

type Packet struct {
	Type               PacketType
	NBinaryAttachments int
	Namespace          string
	Payload            interface{} // array or object
	payloadIsEncoded   bool
	AckId              *int
}

func NewPacket(t PacketType, nBinaryAttachments int, namespace string, jsonSerializablePayload interface{}, ackId *int) Packet {
	return Packet{
		Type:               t,
		NBinaryAttachments: nBinaryAttachments,
		Namespace:          namespace,
		Payload:            jsonSerializablePayload,
		payloadIsEncoded:   false,
		AckId:              ackId,
	}
}

func NewPacketFromEncodedPayload(t PacketType, nBinaryAttachments int, namespace string, payload []byte, ackId *int) Packet {
	return Packet{
		Type:               t,
		NBinaryAttachments: nBinaryAttachments,
		Namespace:          namespace,
		Payload:            payload,
		payloadIsEncoded:   true,
		AckId:              ackId,
	}
}

func (p Packet) encode() ([]byte, error) {
	var buf bytes.Buffer

	// Type
	buf.WriteByte(byte(p.Type) + '0')

	// Binary attachments
	if p.NBinaryAttachments > 0 {
		buf.WriteString(strconv.Itoa(p.NBinaryAttachments) + "-")
	}

	// Namespace
	if p.Namespace != "/" {
		buf.WriteString(p.Namespace + ",")
	}

	// AckId
	if p.AckId != nil {
		buf.WriteString(strconv.Itoa(*p.AckId))
	}

	// Payload
	if p.Payload == nil {
		return buf.Bytes(), errors.New("payload is nil")
	}
	if p.payloadIsEncoded {
		buf.Write(p.Payload.([]byte))
	} else {
		j, err := json.Marshal(p.Payload)
		if err != nil {
			return nil, err
		}
		buf.Write(j)
	}

	return buf.Bytes(), nil
}

func parsePacket(b []byte, decodePayload bool) (Packet, error) {
	ty := PacketType(b[0] - '0')
	if ty < PacketTypeConnect || ty > PacketTypeBinaryAck {
		return Packet{}, errors.New("invalid packet type")
	}

	var (
		bin     int    = 0
		nsp     string = "/"
		ackId   *int
		payload interface{}
	)

	const (
		PosBin = iota
		PosNsp
		PosAckId
	)

	payloadStart := false
	var buf bytes.Buffer
	i := 1
	j := PosBin
	for !payloadStart && i < len(b) {
		// binary attachments
		if b[i] == '-' {
			bufs := buf.String()
			if bufs != "" {
				var err error
				bin, err = strconv.Atoi(bufs)
				if err != nil {
					return Packet{}, err
				}
			}
			buf.Reset()
			i++
			j = PosNsp
			continue
		}

		// namespace
		if b[i] == ',' {
			bufs := buf.String()
			if bufs != "" {
				nsp = bufs
			}
			buf.Reset()
			i++
			j = PosAckId
			continue
		}

		// ackid
		if b[i] == '{' || b[i] == '[' {
			bufs := buf.String()
			if bufs != "" {
				var err error
				ackId = new(int)
				*ackId, err = strconv.Atoi(bufs)
				if err != nil {
					return Packet{}, err
				}
			}
			buf.Reset()
			payloadStart = true
			break
		}

		if b[i] == '/' {
			buf.Reset()
			buf.WriteByte(b[i])
			i++
			j = PosNsp
			continue
		}

		buf.WriteByte(b[i])
		i++
	}

	bufs := buf.String()
	if bufs != "" {
		switch j {
		case PosBin:
			var err error
			bin, err = strconv.Atoi(bufs)
			if err != nil {
				return Packet{}, err
			}
		case PosNsp:
			nsp = bufs
		case PosAckId:
			var err error
			ackId = new(int)
			*ackId, err = strconv.Atoi(bufs)
			if err != nil {
				return Packet{}, err
			}
		}
	}

	payloadStr := b[i:]
	if decodePayload {
		if err := json.Unmarshal(payloadStr, &payload); err != nil {
			return Packet{}, err
		}
	} else {
		payload = payloadStr
	}

	return Packet{
		Type:               ty,
		NBinaryAttachments: bin,
		Namespace:          nsp,
		Payload:            payload,
		payloadIsEncoded:   !decodePayload,
		AckId:              ackId,
	}, nil
}

type connectNamespaceSuccess struct {
	Sid string `json:"sid"`
}

type connectNamespaceFailure struct {
	Message string `json:"message"`
}

func createConnectNamespace(nsp string, sid string) Packet {
	return NewPacket(PacketTypeConnect, 0, nsp, connectNamespaceSuccess{Sid: sid}, nil)
}

func createConnectNamespaceFailure(nsp string, message string) Packet {
	return NewPacket(PacketTypeConnectError, 0, nsp, connectNamespaceFailure{Message: message}, nil)
}
