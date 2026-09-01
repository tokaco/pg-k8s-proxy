package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// buildStartup assembles a raw startup packet the way a client would.
func buildStartup(code int32, params ...string) []byte {
	body := make([]byte, 0, 64)
	body = binary.BigEndian.AppendUint32(body, uint32(code))
	for _, s := range params {
		body = append(body, s...)
		body = append(body, 0)
	}
	body = append(body, 0)

	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	return append(out, body...)
}

func TestReadStartupPacketParsesParametersInOrder(t *testing.T) {
	raw := buildStartup(ProtocolVersion3,
		"user", "alice",
		"database", "billing",
		"application_name", "psql",
	)

	packet, err := ReadStartupPacket(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupPacket: %v", err)
	}

	if packet.Code != ProtocolVersion3 {
		t.Errorf("Code = %d, want %d", packet.Code, ProtocolVersion3)
	}
	if got := packet.MajorVersion(); got != 3 {
		t.Errorf("MajorVersion() = %d, want 3", got)
	}

	want := []Parameter{
		{Key: "user", Value: "alice"},
		{Key: "database", Value: "billing"},
		{Key: "application_name", Value: "psql"},
	}
	if len(packet.Parameters) != len(want) {
		t.Fatalf("got %d parameters, want %d: %+v", len(packet.Parameters), len(want), packet.Parameters)
	}
	for i, w := range want {
		if packet.Parameters[i] != w {
			t.Errorf("Parameters[%d] = %+v, want %+v", i, packet.Parameters[i], w)
		}
	}

	if db, ok := packet.Parameter("database"); !ok || db != "billing" {
		t.Errorf(`Parameter("database") = %q, %v; want "billing", true`, db, ok)
	}
	if _, ok := packet.Parameter("missing"); ok {
		t.Error(`Parameter("missing") reported present`)
	}
}

// Encoding must preserve parameter order: a map would reshuffle it on every
// connection, and some pooling middleware fingerprints the exact bytes.
func TestStartupPacketRoundTripPreservesOrder(t *testing.T) {
	original := buildStartup(ProtocolVersion3,
		"user", "alice",
		"database", "billing",
		"options", "-c statement_timeout=5s",
	)

	packet, err := ReadStartupPacket(bytes.NewReader(original))
	if err != nil {
		t.Fatalf("ReadStartupPacket: %v", err)
	}

	if encoded := packet.Encode(); !bytes.Equal(encoded, original) {
		t.Errorf("Encode() produced different bytes\n got: %q\nwant: %q", encoded, original)
	}
}

func TestSetParameterKeepsPositionAndAppendsNewKeys(t *testing.T) {
	packet := &StartupPacket{
		Code: ProtocolVersion3,
		Parameters: []Parameter{
			{Key: "user", Value: "alice"},
			{Key: "database", Value: "public-name"},
		},
	}

	packet.SetParameter("database", "internal-name")
	if packet.Parameters[1].Key != "database" || packet.Parameters[1].Value != "internal-name" {
		t.Errorf("rewriting database moved or mangled it: %+v", packet.Parameters)
	}

	packet.SetParameter("application_name", "gateway")
	if len(packet.Parameters) != 3 || packet.Parameters[2].Key != "application_name" {
		t.Errorf("new parameter was not appended: %+v", packet.Parameters)
	}
}

func TestReadStartupPacketRecognisesNegotiationCodes(t *testing.T) {
	tests := []struct {
		name  string
		code  int32
		check func(*StartupPacket) bool
	}{
		{"ssl", SSLRequestCode, (*StartupPacket).IsSSLRequest},
		{"gssenc", GSSEncRequestCode, (*StartupPacket).IsGSSEncRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := binary.BigEndian.AppendUint32(nil, 8)
			raw = binary.BigEndian.AppendUint32(raw, uint32(tc.code))

			packet, err := ReadStartupPacket(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("ReadStartupPacket: %v", err)
			}
			if !tc.check(packet) {
				t.Errorf("packet with code %d was not recognised", tc.code)
			}
		})
	}
}

func TestReadStartupPacketDecodesCancelRequest(t *testing.T) {
	want := CancelRequest{ProcessID: 4242, SecretKey: -9}
	raw := EncodeCancelRequest(want)

	packet, err := ReadStartupPacket(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupPacket: %v", err)
	}
	if !packet.IsCancelRequest() {
		t.Fatal("packet was not recognised as a cancel request")
	}
	if packet.Cancel != want {
		t.Errorf("Cancel = %+v, want %+v", packet.Cancel, want)
	}
}

func TestReadStartupPacketRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{
			name: "length below the minimum",
			raw:  binary.BigEndian.AppendUint32(nil, 4),
		},
		{
			name: "length beyond the server limit",
			raw:  binary.BigEndian.AppendUint32(nil, MaxStartupPacketLength+1),
			want: ErrStartupTooLarge,
		},
		{
			name: "truncated body",
			raw:  append(binary.BigEndian.AppendUint32(nil, 32), 0, 0, 0, 3),
		},
		{
			name: "protocol 2.0",
			raw:  buildStartup(2<<16, "user", "alice"),
		},
		{
			name: "cancel request with a short payload",
			raw:  append(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 12), uint32(CancelRequestCode)), 0, 0, 0, 1),
		},
		{
			name: "ssl request with trailing bytes",
			raw:  append(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 12), uint32(SSLRequestCode)), 1, 2, 3, 4),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadStartupPacket(bytes.NewReader(tc.raw))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

// A value missing its terminator must be an error, not a silently truncated
// parameter, since the value could be the database name.
func TestReadStartupPacketRejectsUnterminatedValue(t *testing.T) {
	body := binary.BigEndian.AppendUint32(nil, uint32(ProtocolVersion3))
	body = append(body, "database"...)
	body = append(body, 0)
	body = append(body, "billing"...) // no terminator

	raw := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	raw = append(raw, body...)

	if _, err := ReadStartupPacket(bytes.NewReader(raw)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
}

func TestReadStartupPacketReturnsEOFOnAnEmptyConnection(t *testing.T) {
	// Port probes and TCP health checks connect and hang up immediately; the
	// caller distinguishes that from a protocol error by the EOF.
	if _, err := ReadStartupPacket(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want io.EOF", err)
	}
}

func TestBackendKeyDataRoundTrip(t *testing.T) {
	want := CancelRequest{ProcessID: 1234, SecretKey: 5678}

	encoded := EncodeBackendKeyData(want)
	if encoded[0] != MsgBackendKeyData {
		t.Errorf("message type = %q, want %q", encoded[0], MsgBackendKeyData)
	}

	got, err := DecodeBackendKeyData(encoded[5:])
	if err != nil {
		t.Fatalf("DecodeBackendKeyData: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	if _, err := DecodeBackendKeyData([]byte{1, 2, 3}); err == nil {
		t.Error("expected an error for a short BackendKeyData body")
	}
}

func TestEncodeErrorResponseCarriesTheSQLState(t *testing.T) {
	raw := EncodeErrorResponse(SQLStateUndefinedDatabase, `database "nope" does not exist`, "No route claims it.")

	if raw[0] != MsgErrorResponse {
		t.Fatalf("message type = %q, want %q", raw[0], MsgErrorResponse)
	}
	length := binary.BigEndian.Uint32(raw[1:5])
	if int(length)+1 != len(raw) {
		t.Errorf("declared length %d does not match the %d byte message", length, len(raw))
	}

	body := string(raw[5:])
	for _, want := range []string{"FATAL", SQLStateUndefinedDatabase, `database "nope" does not exist`, "No route claims it."} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q: %q", want, body)
		}
	}
	if body[len(body)-1] != 0 {
		t.Error("body is not terminated by a null byte")
	}
}
