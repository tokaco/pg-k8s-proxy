package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MaxMessageLength caps a single relayed backend message. The startup phase
// never carries anything close to this; it exists so a malformed or hostile
// length field cannot make the proxy allocate without bound.
const MaxMessageLength = 1 << 20 // 1 MiB

// Message is a single framed protocol message: a type byte followed by a
// length-prefixed body. Body aliases the reader's buffer and is only valid
// until the next read.
type Message struct {
	Type byte
	Body []byte
}

// Encode serialises the message back onto the wire.
func (m Message) Encode() []byte {
	buf := make([]byte, 0, 5+len(m.Body))
	buf = append(buf, m.Type)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(m.Body)+4))
	return append(buf, m.Body...)
}

// MessageReader frames the typed messages a backend sends after the startup
// packet. It reads exactly as many bytes as each message occupies and never
// reads ahead, so a caller can stop framing partway through a stream and hand
// the underlying connection to a raw byte splice without losing data.
type MessageReader struct {
	r   io.Reader
	buf []byte // holds the current message body
}

// NewMessageReader wraps r.
func NewMessageReader(r io.Reader) *MessageReader {
	return &MessageReader{r: r}
}

// Next reads one message. The returned Body is only valid until the following
// call to Next.
func (mr *MessageReader) Next() (Message, error) {
	var header [5]byte
	if _, err := io.ReadFull(mr.r, header[:]); err != nil {
		return Message{}, err
	}

	// The length field counts itself but not the type byte.
	length := int64(binary.BigEndian.Uint32(header[1:]))
	if length < 4 {
		return Message{}, fmt.Errorf("pgwire: message %q declares length %d, want at least 4", header[0], length)
	}
	if length > MaxMessageLength {
		return Message{}, fmt.Errorf("pgwire: message %q length %d exceeds the %d byte limit", header[0], length, MaxMessageLength)
	}

	bodyLen := int(length - 4)
	if cap(mr.buf) < bodyLen {
		mr.buf = make([]byte, bodyLen)
	}
	mr.buf = mr.buf[:bodyLen]
	if _, err := io.ReadFull(mr.r, mr.buf); err != nil {
		return Message{}, fmt.Errorf("pgwire: reading body of message %q: %w", header[0], err)
	}

	return Message{Type: header[0], Body: mr.buf}, nil
}

// SQLSTATE codes the gateway reports on its own behalf.
const (
	// SQLStateUndefinedDatabase is what a real server returns for an unknown
	// database, so clients already handle it.
	SQLStateUndefinedDatabase = "3D000"
	// SQLStateConnectionFailure reports that the backend could not be reached.
	SQLStateConnectionFailure = "08006"
	// SQLStateTooManyConnections reports that the gateway is at its limit.
	SQLStateTooManyConnections = "53300"
	// SQLStateProtocolViolation reports a malformed client handshake.
	SQLStateProtocolViolation = "08P01"
)

// EncodeErrorResponse builds a fatal ErrorResponse. Sending one before closing
// gives clients a real diagnostic instead of an unexpected EOF.
func EncodeErrorResponse(sqlState, message, detail string) []byte {
	fields := []struct {
		code  byte
		value string
	}{
		{'S', "FATAL"},
		{'V', "FATAL"},
		{'C', sqlState},
		{'M', message},
	}
	if detail != "" {
		fields = append(fields, struct {
			code  byte
			value string
		}{'D', detail})
	}

	body := make([]byte, 0, 64)
	for _, f := range fields {
		body = append(body, f.code)
		body = append(body, f.value...)
		body = append(body, 0)
	}
	body = append(body, 0) // terminator

	return Message{Type: MsgErrorResponse, Body: body}.Encode()
}
