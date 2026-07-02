package runnerproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestPreambleFrameIsLengthPrefixedStableJSON(t *testing.T) {
	var buf bytes.Buffer
	preamble := NewPreamble("run-1", "test-bin")
	if err := WritePreamble(&buf, preamble); err != nil {
		t.Fatal(err)
	}

	raw := buf.Bytes()
	if len(raw) < 5 {
		t.Fatalf("frame too short: %d", len(raw))
	}
	size := binary.BigEndian.Uint32(raw[:4])
	if int(size) != len(raw[4:]) {
		t.Fatalf("length prefix = %d, body length = %d", size, len(raw[4:]))
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw[4:], &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["magic"] != Magic {
		t.Fatalf("magic = %v", decoded["magic"])
	}
	if decoded["runner_protocol_version"] != float64(ProtocolVersion) {
		t.Fatalf("version = %v", decoded["runner_protocol_version"])
	}
	if decoded["runner_binary_version"] != "test-bin" {
		t.Fatalf("binary version = %v", decoded["runner_binary_version"])
	}
	if decoded["run_id"] != "run-1" {
		t.Fatalf("run id = %v", decoded["run_id"])
	}
}

func TestReadMessageDecodesSendInputBytesAndAck(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, NewSendInput(7, []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(&buf, NewAck(7)); err != nil {
		t.Fatal(err)
	}

	first, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	input, ok := first.(SendInput)
	if !ok {
		t.Fatalf("first message type = %T", first)
	}
	if input.Seq != 7 || string(input.Bytes) != "hello\n" {
		t.Fatalf("input = %+v", input)
	}

	second, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := second.(Ack)
	if !ok {
		t.Fatalf("second message type = %T", second)
	}
	if ack.Seq != 7 {
		t.Fatalf("ack seq = %d", ack.Seq)
	}
}

func TestReadMessageDecodesSubscribeAndHeartbeat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, NewSubscribe(true, 12)); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(&buf, NewHeartbeat("run-1")); err != nil {
		t.Fatal(err)
	}

	first, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	subscribe, ok := first.(Subscribe)
	if !ok {
		t.Fatalf("first message type = %T", first)
	}
	if !subscribe.Output || subscribe.FromChunkSeq != 12 {
		t.Fatalf("subscribe = %+v", subscribe)
	}

	second, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, ok := second.(Heartbeat)
	if !ok {
		t.Fatalf("second message type = %T", second)
	}
	if heartbeat.RunID != "run-1" || heartbeat.ProtocolVersion != ProtocolVersion {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
}

func TestReadMessageDecodesOutputChunk(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, NewOutputChunk(4, "Stdout", []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok := msg.(OutputChunk)
	if !ok {
		t.Fatalf("message type = %T", msg)
	}
	if chunk.ChunkSeq != 4 || chunk.Stream != "Stdout" || string(chunk.Bytes) != "hello\n" {
		t.Fatalf("chunk = %+v", chunk)
	}
}

func TestExitPreservesZeroExitCode(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, NewExit(ProviderExited(0))); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	exit, ok := msg.(Exit)
	if !ok {
		t.Fatalf("message type = %T", msg)
	}
	if exit.Termination.ExitCode == nil || *exit.Termination.ExitCode != 0 {
		t.Fatalf("exit code = %v", exit.Termination.ExitCode)
	}
}

func TestWriteFrameHandlesShortWrites(t *testing.T) {
	writer := &shortWriter{limit: 2}
	if err := WriteMessage(writer, NewAck(3)); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(bytes.NewReader(writer.bytes.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := msg.(Ack)
	if !ok {
		t.Fatalf("message type = %T", msg)
	}
	if ack.Seq != 3 {
		t.Fatalf("ack seq = %d", ack.Seq)
	}
}

type shortWriter struct {
	limit int
	bytes bytes.Buffer
}

func (w *shortWriter) Write(raw []byte) (int, error) {
	if len(raw) > w.limit {
		raw = raw[:w.limit]
	}
	return w.bytes.Write(raw)
}
