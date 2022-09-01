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

func NewPacket(t PacketType, nBinaryAttachments int, namespace string, payload interface{}, ackId *int) Packet {
	return Packet{
		Type:               t,
		NBinaryAttachments: nBinaryAttachments,
		Namespace:          namespace,
		Payload:            payload,
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
		bin     int
		nsp     string
		ackId   *int
		payload interface{}
	)

	payloadStart := false
	var buf bytes.Buffer
	i := 1
	for !payloadStart && i < len(b) {
		// binary attachments
		if b[i] == '-' {
			var err error
			bin, err = strconv.Atoi(buf.String())
			if err != nil {
				return Packet{}, err
			}
			buf.Reset()
			continue
		}

		// namespace
		if b[i] == ',' {
			nsp = buf.String()
			buf.Reset()
			continue
		}

		// ackid
		if b[i] == '{' || b[i] == '[' {
			var err error
			ackId = new(int)
			*ackId, err = strconv.Atoi(buf.String())
			if err != nil {
				return Packet{}, err
			}
			buf.Reset()
			payloadStart = true
			continue
		}

		buf.WriteByte(b[i])
		i++
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
