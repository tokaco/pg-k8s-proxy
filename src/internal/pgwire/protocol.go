// Package pgwire implements the slice of the PostgreSQL frontend/backend
// protocol a routing gateway needs: the startup handshake, cancellation
// requests, and enough of the backend message framing to intercept the
// BackendKeyData that makes query cancellation work across the proxy.
//
// Reference: https://www.postgresql.org/docs/current/protocol.html
package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Protocol codes carried in the version field of a startup packet.
const (
	// ProtocolVersion3 is the "3.0" protocol every supported server speaks.
	ProtocolVersion3 int32 = 196608 // 3<<16 | 0

	// CancelRequestCode marks a packet asking to cancel a running query.
	CancelRequestCode int32 = 80877102 // 1234<<16 | 5678
	// SSLRequestCode marks a packet asking to upgrade the connection to TLS.
	SSLRequestCode int32 = 80877103 // 1234<<16 | 5679
	// GSSEncRequestCode marks a packet asking for GSSAPI encryption.
	GSSEncRequestCode int32 = 80877104 // 1234<<16 | 5680
)

// Backend message types this package needs to recognise. The full set is much
// larger; everything else is relayed without inspection.
const (
	// MsgBackendKeyData carries the (pid, secret) pair used to cancel queries.
	MsgBackendKeyData byte = 'K'
	// MsgReadyForQuery marks the end of the startup phase.
	MsgReadyForQuery byte = 'Z'
	// MsgErrorResponse reports a fatal or non-fatal error.
	MsgErrorResponse byte = 'E'
)

// MaxStartupPacketLength mirrors the server's own limit (PG_MAX_STARTUP_PACKET_LENGTH).
const MaxStartupPacketLength = 10000

// minStartupPacketLength is the length field plus the version field.
const minStartupPacketLength = 8

// ErrStartupTooLarge is returned when a peer announces an implausible packet.
var ErrStartupTooLarge = errors.New("pgwire: startup packet exceeds maximum length")

// Parameter is one key/value pair from a startup packet. Parameters are kept in
// an ordered slice rather than a map so that the packet forwarded to the backend
// is byte-for-byte equivalent to the one the client sent.
type Parameter struct {
	Key   string
	Value string
}

// StartupPacket is a decoded startup, SSL, GSSAPI, or cancel packet. Exactly one
// of Parameters or Cancel is meaningful, depending on Code.
type StartupPacket struct {
	// Code is the protocol version or one of the request codes above.
	Code int32
	// Parameters is set for a regular protocol 3.0 startup packet.
	Parameters []Parameter
	// Cancel is set when Code is CancelRequestCode.
	Cancel CancelRequest
}

// CancelRequest identifies the backend session a client wants to cancel.
type CancelRequest struct {
	ProcessID int32
	SecretKey int32
}

// IsSSLRequest reports whether the client asked to negotiate TLS.
func (p *StartupPacket) IsSSLRequest() bool { return p.Code == SSLRequestCode }

// IsGSSEncRequest reports whether the client asked to negotiate GSSAPI encryption.
func (p *StartupPacket) IsGSSEncRequest() bool { return p.Code == GSSEncRequestCode }

// IsCancelRequest reports whether the client asked to cancel a query.
func (p *StartupPacket) IsCancelRequest() bool { return p.Code == CancelRequestCode }

// MajorVersion returns the protocol major version encoded in Code.
func (p *StartupPacket) MajorVersion() int32 { return p.Code >> 16 }

// Parameter returns the value of a startup parameter and whether it was present.
func (p *StartupPacket) Parameter(key string) (string, bool) {
	for _, param := range p.Parameters {
		if param.Key == key {
			return param.Value, true
		}
	}
	return "", false
}

// SetParameter replaces the value of an existing parameter, or appends it. The
// position of an existing key is preserved.
func (p *StartupPacket) SetParameter(key, value string) {
	for i := range p.Parameters {
		if p.Parameters[i].Key == key {
			p.Parameters[i].Value = value
			return
		}
	}
	p.Parameters = append(p.Parameters, Parameter{Key: key, Value: value})
}

// ReadStartupPacket decodes a single startup-phase packet. Unlike regular
// protocol messages, startup packets carry no type byte: the first four bytes
// are the total length, the next four the version or request code.
func ReadStartupPacket(r io.Reader) (*StartupPacket, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	length := int64(binary.BigEndian.Uint32(header[:]))
	if length < minStartupPacketLength {
		return nil, fmt.Errorf("pgwire: startup packet length %d is below the minimum of %d", length, minStartupPacketLength)
	}
	if length > MaxStartupPacketLength {
		return nil, fmt.Errorf("%w: %d > %d", ErrStartupTooLarge, length, MaxStartupPacketLength)
	}

	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("pgwire: reading startup packet body: %w", err)
	}

	packet := &StartupPacket{Code: int32(binary.BigEndian.Uint32(body[:4]))}
	payload := body[4:]

	switch packet.Code {
	case SSLRequestCode, GSSEncRequestCode:
		if len(payload) != 0 {
			return nil, fmt.Errorf("pgwire: negotiation packet carries %d trailing bytes", len(payload))
		}
		return packet, nil

	case CancelRequestCode:
		if len(payload) != 8 {
			return nil, fmt.Errorf("pgwire: cancel request payload is %d bytes, want 8", len(payload))
		}
		packet.Cancel = CancelRequest{
			ProcessID: int32(binary.BigEndian.Uint32(payload[:4])),
			SecretKey: int32(binary.BigEndian.Uint32(payload[4:])),
		}
		return packet, nil
	}

	if packet.MajorVersion() != 3 {
		return nil, fmt.Errorf("pgwire: unsupported protocol version %d.%d", packet.MajorVersion(), packet.Code&0xFFFF)
	}

	params, err := parseParameters(payload)
	if err != nil {
		return nil, err
	}
	packet.Parameters = params
	return packet, nil
}

// parseParameters decodes the null-terminated key/value sequence that follows
// the version field, terminated by an empty key.
func parseParameters(payload []byte) ([]Parameter, error) {
	var params []Parameter
	for len(payload) > 0 {
		// An empty key terminates the list; trailing padding is tolerated
		// because some clients align the packet.
		if payload[0] == 0 {
			return params, nil
		}

		key, rest, err := readCString(payload)
		if err != nil {
			return nil, fmt.Errorf("pgwire: reading parameter name: %w", err)
		}
		value, rest, err := readCString(rest)
		if err != nil {
			return nil, fmt.Errorf("pgwire: reading value of parameter %q: %w", key, err)
		}
		params = append(params, Parameter{Key: key, Value: value})
		payload = rest
	}
	return params, nil
}

func readCString(b []byte) (value string, rest []byte, err error) {
	end := bytes.IndexByte(b, 0)
	if end == -1 {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(b[:end]), b[end+1:], nil
}

// Encode serialises a protocol 3.0 startup packet, preserving parameter order.
func (p *StartupPacket) Encode() []byte {
	size := 4 + 4 + 1 // length + version + list terminator
	for _, param := range p.Parameters {
		size += len(param.Key) + 1 + len(param.Value) + 1
	}

	buf := make([]byte, 0, size)
	buf = binary.BigEndian.AppendUint32(buf, uint32(size))
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.Code))
	for _, param := range p.Parameters {
		buf = append(buf, param.Key...)
		buf = append(buf, 0)
		buf = append(buf, param.Value...)
		buf = append(buf, 0)
	}
	return append(buf, 0)
}

// EncodeCancelRequest serialises a cancel packet for the given backend key.
func EncodeCancelRequest(key CancelRequest) []byte {
	buf := make([]byte, 0, 16)
	buf = binary.BigEndian.AppendUint32(buf, 16)
	buf = binary.BigEndian.AppendUint32(buf, uint32(CancelRequestCode))
	buf = binary.BigEndian.AppendUint32(buf, uint32(key.ProcessID))
	return binary.BigEndian.AppendUint32(buf, uint32(key.SecretKey))
}

// EncodeBackendKeyData serialises a BackendKeyData message.
func EncodeBackendKeyData(key CancelRequest) []byte {
	buf := make([]byte, 0, 13)
	buf = append(buf, MsgBackendKeyData)
	buf = binary.BigEndian.AppendUint32(buf, 12) // length excludes the type byte
	buf = binary.BigEndian.AppendUint32(buf, uint32(key.ProcessID))
	return binary.BigEndian.AppendUint32(buf, uint32(key.SecretKey))
}

// DecodeBackendKeyData reads the (pid, secret) pair from a BackendKeyData body.
func DecodeBackendKeyData(body []byte) (CancelRequest, error) {
	if len(body) != 8 {
		return CancelRequest{}, fmt.Errorf("pgwire: BackendKeyData body is %d bytes, want 8", len(body))
	}
	return CancelRequest{
		ProcessID: int32(binary.BigEndian.Uint32(body[:4])),
		SecretKey: int32(binary.BigEndian.Uint32(body[4:])),
	}, nil
}
