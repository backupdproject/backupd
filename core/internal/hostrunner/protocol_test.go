package hostrunner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// TestReadMessage_RefusesAFrameThatNamesAPath is the protocol's central
// security claim, tested rather than asserted in a comment.
//
// The whole design rests on "a path cannot cross this socket". That is
// true only because the decoder refuses fields it does not know: a
// tolerant decoder would drop script_path silently, the request would be
// well-formed without it, and a future field with the same name would
// then be live surface nobody added on purpose.
func TestReadMessage_RefusesAFrameThatNamesAPath(t *testing.T) {
	frame := frameOf(t, `{"kind":"request","request":{"op":"execute","run_id":"r","step_id":"s","script_path":"/etc/shadow"}}`)

	_, err := ReadMessage(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("a frame carrying script_path was accepted. The runner would then execute the rest of the request while ignoring the field, which is how a protocol that 'cannot express a path' quietly starts to.")
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("the refusal is not an ErrProtocol, so a caller cannot tell a malformed frame from a transport failure: %v", err)
	}
	if !strings.Contains(err.Error(), "script_path") {
		t.Errorf("the refusal does not name the field that caused it, so nobody can act on it: %v", err)
	}
}

// TestReadMessage_RefusesAFramePastTheBoundWithoutAllocatingIt checks the
// length prefix is a bound and not a description.
//
// A four-gigabyte length on a local socket is a one-line
// denial-of-service against a process that is holding a backup window
// open, and the check has to happen between reading the prefix and
// allocating the body -- which is the only reason the prefix is read
// separately at all.
func TestReadMessage_RefusesAFramePastTheBoundWithoutAllocatingIt(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(MaxFrameSize+1))

	// Only the header is supplied. A reader that allocated first and
	// bounded afterwards would block trying to fill the body; one that
	// bounds first refuses immediately, which is what this asserts.
	_, err := ReadMessage(bytes.NewReader(header[:]))
	if err == nil {
		t.Fatal("a frame claiming more than MaxFrameSize bytes was accepted")
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("the refusal is not an ErrProtocol: %v", err)
	}
}

// TestMessage_CarriesArbitraryHookOutputUnchanged is the framing's other
// job.
//
// A hook's stderr is whatever a tool wrote: a tar progress line with a
// carriage return, a NUL from a binary, an invalid UTF-8 sequence from a
// filename in some other encoding. A newline-delimited or text-assuming
// framing corrupts exactly those, and it corrupts them in the evidence an
// operator reads when something has gone wrong.
func TestMessage_CarriesArbitraryHookOutputUnchanged(t *testing.T) {
	raw := []byte{0x00, 0x0a, 0x0d, 0xff, 0xfe, '"', '\\', '{', '}'}

	var buf bytes.Buffer
	if err := WriteMessage(&buf, Message{Kind: KindChunk, Chunk: &Chunk{Stream: StreamStderr, Seq: 7, Data: raw}}); err != nil {
		t.Fatalf("writing a chunk of arbitrary bytes: %v", err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if msg.Chunk == nil {
		t.Fatal("the chunk did not survive the round trip at all")
	}
	if !bytes.Equal(msg.Chunk.Data, raw) {
		t.Errorf("the bytes changed in transit: sent %q, received %q", raw, msg.Chunk.Data)
	}
	if msg.Chunk.Stream != StreamStderr || msg.Chunk.Seq != 7 {
		t.Errorf("the chunk's identity changed: stream %v seq %d", msg.Chunk.Stream, msg.Chunk.Seq)
	}
}

// TestReadMessage_RefusesTwoValuesInOneFrame closes the trailing-garbage
// case: a frame whose length covers a second JSON value would otherwise
// be decoded as its first, with the rest silently unread.
func TestReadMessage_RefusesTwoValuesInOneFrame(t *testing.T) {
	frame := frameOf(t, `{"kind":"hello"}{"kind":"hello"}`)

	if _, err := ReadMessage(bytes.NewReader(frame)); err == nil {
		t.Fatal("a frame carrying two JSON values was accepted, so half of it was ignored")
	}
}

// TestReadMessage_RefusesAFrameThatIsTwoThingsAtOnce is the tagged-union
// invariant, which DisallowUnknownFields does not cover.
//
// Every arm named below is a field this envelope really has, so a
// request frame that also carries a failure decodes without complaint
// and the extra arm is dropped by whichever branch reads only what its
// kind told it to expect. Two readers of one frame then disagree about
// what arrived, which is the shape every parser-differential bug has.
func TestReadMessage_RefusesAFrameThatIsTwoThingsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			"a request smuggling a second arm",
			`{"kind":"request","request":{"op":"status"},"failure":{"code":"refused","message":"x"}}`,
		},
		{
			"a hello smuggling a result",
			`{"kind":"hello","hello":{"protocol":"` + Protocol + `","version":"v","token":"t"},"result":{"state":"exited"}}`,
		},
		{
			"a kind that does not match the arm it carries",
			`{"kind":"request","failure":{"code":"refused","message":"x"}}`,
		},
		{
			"a kind with no arm at all",
			`{"kind":"request"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := ReadMessage(bytes.NewReader(frameOf(t, tc.body)))
			if err == nil {
				t.Fatalf("a frame the protocol has no single meaning for was accepted as %+v", msg)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("the refusal is not an ErrProtocol, so a caller cannot tell it from a transport failure: %v", err)
			}
		})
	}
}

func frameOf(t *testing.T, body string) []byte {
	t.Helper()
	var out bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	out.Write(header[:])
	out.WriteString(body)
	return out.Bytes()
}
