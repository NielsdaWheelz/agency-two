package runnerproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ProtocolVersion = 1
	Magic           = "agency.runner"
	MaxFrameBytes   = 8 << 20
)

type Preamble struct {
	Magic                 string `json:"magic"`
	RunnerProtocolVersion int    `json:"runner_protocol_version"`
	RunnerBinaryVersion   string `json:"runner_binary_version"`
	RunID                 string `json:"run_id"`
}

func NewPreamble(runID, binaryVersion string) Preamble {
	return Preamble{
		Magic:                 Magic,
		RunnerProtocolVersion: ProtocolVersion,
		RunnerBinaryVersion:   binaryVersion,
		RunID:                 runID,
	}
}

func (p Preamble) Validate() error {
	if p.Magic != Magic {
		return fmt.Errorf("runner preamble magic %q does not match %q", p.Magic, Magic)
	}
	if p.RunnerProtocolVersion <= 0 {
		return errors.New("runner preamble protocol version must be positive")
	}
	if p.RunID == "" {
		return errors.New("runner preamble run_id is required")
	}
	return nil
}

type MessageType string

const (
	MessageHello       MessageType = "Hello"
	MessageSubscribe   MessageType = "Subscribe"
	MessageHeartbeat   MessageType = "Heartbeat"
	MessageOutputChunk MessageType = "OutputChunk"
	MessageSendInput   MessageType = "SendInput"
	MessageAck         MessageType = "Ack"
	MessageNack        MessageType = "Nack"
	MessageStop        MessageType = "Stop"
	MessageKill        MessageType = "Kill"
	MessageExit        MessageType = "Exit"
)

type Hello struct {
	Type    MessageType `json:"type"`
	Version int         `json:"version"`
}

func NewHello() Hello {
	return Hello{Type: MessageHello, Version: ProtocolVersion}
}

type Subscribe struct {
	Type         MessageType `json:"type"`
	Output       bool        `json:"output"`
	FromChunkSeq int64       `json:"fromChunkSeq"`
}

func NewSubscribe(output bool, fromChunkSeq int64) Subscribe {
	return Subscribe{Type: MessageSubscribe, Output: output, FromChunkSeq: fromChunkSeq}
}

type Heartbeat struct {
	Type            MessageType `json:"type"`
	RunID           string      `json:"runId"`
	ProtocolVersion int         `json:"protocolVersion"`
}

func NewHeartbeat(runID string) Heartbeat {
	return Heartbeat{Type: MessageHeartbeat, RunID: runID, ProtocolVersion: ProtocolVersion}
}

type OutputChunk struct {
	Type     MessageType `json:"type"`
	ChunkSeq int64       `json:"chunkSeq"`
	Stream   string      `json:"stream"`
	Bytes    []byte      `json:"bytes"`
}

func NewOutputChunk(seq int64, stream string, bytes []byte) OutputChunk {
	return OutputChunk{Type: MessageOutputChunk, ChunkSeq: seq, Stream: stream, Bytes: bytes}
}

type SendInput struct {
	Type  MessageType `json:"type"`
	Seq   int64       `json:"seq"`
	Bytes []byte      `json:"bytes"`
}

func NewSendInput(seq int64, bytes []byte) SendInput {
	return SendInput{Type: MessageSendInput, Seq: seq, Bytes: bytes}
}

type Ack struct {
	Type MessageType `json:"type"`
	Seq  int64       `json:"seq"`
}

func NewAck(seq int64) Ack {
	return Ack{Type: MessageAck, Seq: seq}
}

type Nack struct {
	Type   MessageType `json:"type"`
	Seq    int64       `json:"seq"`
	Code   string      `json:"code"`
	Detail string      `json:"detail,omitempty"`
}

func NewNack(seq int64, code, detail string) Nack {
	return Nack{Type: MessageNack, Seq: seq, Code: code, Detail: detail}
}

type Stop struct {
	Type     MessageType `json:"type"`
	Graceful bool        `json:"graceful"`
}

func NewStop(graceful bool) Stop {
	return Stop{Type: MessageStop, Graceful: graceful}
}

type Kill struct {
	Type MessageType `json:"type"`
}

func NewKill() Kill {
	return Kill{Type: MessageKill}
}

type Exit struct {
	Type        MessageType `json:"type"`
	Termination Termination `json:"termination"`
}

func NewExit(termination Termination) Exit {
	return Exit{Type: MessageExit, Termination: termination}
}

type Termination struct {
	Outcome  string   `json:"outcome"`
	ExitCode *int     `json:"exitCode,omitempty"`
	Signal   string   `json:"signal,omitempty"`
	Failure  *Failure `json:"failure,omitempty"`
}

type Failure struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func ProviderExited(code int) Termination {
	return Termination{Outcome: "ProviderExited", ExitCode: &code}
}

func UserStopped() Termination {
	return Termination{Outcome: "UserStopped"}
}

func UserKilled(signal string) Termination {
	return Termination{Outcome: "UserKilled", Signal: signal}
}

func RunnerFailed(code, detail string) Termination {
	return Termination{
		Outcome: "RunnerFailed",
		Failure: &Failure{
			Code:   code,
			Detail: detail,
		},
	}
}

func StartFailed(code, detail string) Termination {
	return Termination{
		Outcome: "StartFailed",
		Failure: &Failure{
			Code:   code,
			Detail: detail,
		},
	}
}

type envelope struct {
	Type MessageType `json:"type"`
}

func WritePreamble(w io.Writer, p Preamble) error {
	return WriteFrame(w, p)
}

func ReadPreamble(r io.Reader) (Preamble, error) {
	var p Preamble
	if err := ReadFrame(r, &p); err != nil {
		return Preamble{}, err
	}
	if err := p.Validate(); err != nil {
		return Preamble{}, err
	}
	return p, nil
}

func WriteMessage(w io.Writer, msg any) error {
	return WriteFrame(w, msg)
}

func ReadMessage(r io.Reader) (any, error) {
	raw, err := ReadRawFrame(r)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	switch env.Type {
	case MessageHello:
		var msg Hello
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageSubscribe:
		var msg Subscribe
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageHeartbeat:
		var msg Heartbeat
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageOutputChunk:
		var msg OutputChunk
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageSendInput:
		var msg SendInput
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageAck:
		var msg Ack
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageNack:
		var msg Nack
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageStop:
		var msg Stop
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageKill:
		var msg Kill
		err = json.Unmarshal(raw, &msg)
		return msg, err
	case MessageExit:
		var msg Exit
		err = json.Unmarshal(raw, &msg)
		return msg, err
	default:
		return nil, fmt.Errorf("unknown runner message type %q", env.Type)
	}
}

func WriteFrame(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > MaxFrameBytes {
		return fmt.Errorf("runner frame is %d bytes, max is %d", len(raw), MaxFrameBytes)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	if err := writeFull(w, prefix[:]); err != nil {
		return err
	}
	return writeFull(w, raw)
}

func ReadFrame(r io.Reader, value any) error {
	raw, err := ReadRawFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}

func ReadRawFrame(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 {
		return nil, errors.New("runner frame length must be positive")
	}
	if size > MaxFrameBytes {
		return nil, fmt.Errorf("runner frame length %d exceeds max %d", size, MaxFrameBytes)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func writeFull(w io.Writer, raw []byte) error {
	for len(raw) > 0 {
		n, err := w.Write(raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	return nil
}
