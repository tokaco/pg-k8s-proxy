package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestMessageReaderFramesMessagesAndStopsExactlyAtTheBoundary(t *testing.T) {
	// Two framed messages followed by raw bytes. The reader must consume only
	// the framed prefix, because the proxy hands the remainder to a byte splice.
	var stream bytes.Buffer
	stream.Write(Message{Type: 'R', Body: []byte{0, 0, 0, 0}}.Encode())
	stream.Write(EncodeBackendKeyData(CancelRequest{ProcessID: 7, SecretKey: 9}))
	const trailer = "raw bytes after the handshake"
	stream.WriteString(trailer)

	reader := NewMessageReader(&stream)

	first, err := reader.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if first.Type != 'R' {
		t.Errorf("first message type = %q, want 'R'", first.Type)
	}

	second, err := reader.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if second.Type != MsgBackendKeyData {
		t.Errorf("second message type = %q, want %q", second.Type, MsgBackendKeyData)
	}
	key, err := DecodeBackendKeyData(second.Body)
	if err != nil {
		t.Fatalf("DecodeBackendKeyData: %v", err)
	}
	if want := (CancelRequest{ProcessID: 7, SecretKey: 9}); key != want {
		t.Errorf("key = %+v, want %+v", key, want)
	}

	rest, err := io.ReadAll(&stream)
	if err != nil {
		t.Fatalf("draining the stream: %v", err)
	}
	if string(rest) != trailer {
		t.Errorf("reader over-consumed the stream: %q remained, want %q", rest, trailer)
	}
}

func TestMessageEncodeRoundTrip(t *testing.T) {
	original := Message{Type: 'Z', Body: []byte{'I'}}

	reader := NewMessageReader(bytes.NewReader(original.Encode()))
	got, err := reader.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Type != original.Type || !bytes.Equal(got.Body, original.Body) {
		t.Errorf("round trip = %+v, want %+v", got, original)
	}
}

func TestMessageReaderRejectsImplausibleLengths(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "length below the header size",
			raw:  append([]byte{'Z'}, binary.BigEndian.AppendUint32(nil, 3)...),
		},
		{
			name: "length beyond the allocation cap",
			raw:  append([]byte{'D'}, binary.BigEndian.AppendUint32(nil, MaxMessageLength+1)...),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMessageReader(bytes.NewReader(tc.raw)).Next(); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestMessageReaderReportsEOFOnACleanClose(t *testing.T) {
	if _, err := NewMessageReader(bytes.NewReader(nil)).Next(); !errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want io.EOF", err)
	}
}
