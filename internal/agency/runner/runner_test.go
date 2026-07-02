package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agency-two/internal/agency/runnerproto"
)

// syncBuffer is a goroutine-safe io.Writer used where the runner's output
// goroutine writes while the test goroutine inspects the accumulated bytes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	return string(b.Bytes())
}

func TestRunnerServesPreambleAndAcksInputAfterPTYWrite(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runner.sock")
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-ack",
			SocketPath:    socket,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "read line; printf 'got:%s\\n' \"$line\""},
			Stdout:        &output,
			Stderr:        &bytes.Buffer{},
		})
	}()

	conn := dialRunner(t, socket, errs)
	defer conn.Close()
	preamble, err := runnerproto.ReadPreamble(conn)
	if err != nil {
		t.Fatal(err)
	}
	if preamble.RunID != "run-ack" || preamble.RunnerProtocolVersion != runnerproto.ProtocolVersion {
		t.Fatalf("preamble = %+v", preamble)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewHello()); err != nil {
		t.Fatal(err)
	}
	msg, err := runnerproto.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, ok := msg.(runnerproto.Heartbeat)
	if !ok {
		t.Fatalf("heartbeat message type = %T", msg)
	}
	if heartbeat.RunID != "run-ack" {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSendInput(1, []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	msg, err = runnerproto.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := msg.(runnerproto.Ack)
	if !ok {
		t.Fatalf("ack message type = %T", msg)
	}
	if ack.Seq != 1 {
		t.Fatalf("ack seq = %d", ack.Seq)
	}

	exit := readExit(t, conn)
	if exit.Termination.Outcome != "ProviderExited" {
		t.Fatalf("exit = %+v", exit)
	}
	eventually(t, func() bool {
		return bytes.Contains(output.Bytes(), []byte("got:hello"))
	})

	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit")
	}
}

func TestRunnerDuplicateInputSeqDoesNotWriteTwice(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runner.sock")
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-duplicate-input",
			SocketPath:    socket,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "read first; printf 'first:%s\\n' \"$first\"; read second; printf 'second:%s\\n' \"$second\"; sleep 30"},
			Stdout:        &output,
			Stderr:        &bytes.Buffer{},
		})
	}()

	conn := dialRunner(t, socket, errs)
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		t.Fatal(err)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSendInput(1, []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	readAck(t, conn, 1)
	eventually(t, func() bool {
		return bytes.Contains(output.Bytes(), []byte("first:hello"))
	})
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSendInput(1, []byte("hello\n"))); err != nil {
		t.Fatal(err)
	}
	readAck(t, conn, 1)
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSendInput(2, []byte("world\n"))); err != nil {
		t.Fatal(err)
	}
	readAck(t, conn, 2)
	eventually(t, func() bool {
		return bytes.Contains(output.Bytes(), []byte("second:world"))
	})
	if bytes.Contains(output.Bytes(), []byte("second:hello")) {
		t.Fatalf("duplicate input seq reached child PTY:\n%s", output.String())
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewKill()); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerKillStopsChildAndReportsKilled(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runner.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-kill",
			SocketPath:    socket,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "trap '' TERM; sleep 30"},
			Stdout:        &bytes.Buffer{},
			Stderr:        &bytes.Buffer{},
		})
	}()

	conn := dialRunner(t, socket, errs)
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		t.Fatal(err)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewKill()); err != nil {
		t.Fatal(err)
	}
	exit := readExit(t, conn)
	if exit.Termination.Outcome != "UserKilled" {
		t.Fatalf("exit = %+v", exit)
	}

	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit")
	}
}

func TestRunnerSubscribeReplaysCapturedOutputAndWritesSpool(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "runner.sock")
	spool := filepath.Join(dir, "runner.out")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-output",
			SocketPath:    socket,
			SpoolPath:     spool,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "printf 'first\\n'; sleep 30"},
			Stdout:        io.Discard,
			Stderr:        &bytes.Buffer{},
		})
	}()

	eventually(t, func() bool {
		raw, err := os.ReadFile(spool)
		return err == nil && bytes.Contains(raw, []byte("first"))
	})

	conn := dialRunner(t, socket, errs)
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		t.Fatal(err)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSubscribe(true, 1)); err != nil {
		t.Fatal(err)
	}
	var replayed bytes.Buffer
	for !bytes.Contains(replayed.Bytes(), []byte("first")) {
		chunk := readOutputChunk(t, conn)
		if chunk.Stream != "Stdout" {
			t.Fatalf("chunk = %+v", chunk)
		}
		replayed.Write(chunk.Bytes)
	}
	rawSpool, err := os.ReadFile(spool)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rawSpool, []byte("first")) {
		t.Fatalf("spool = %q", string(rawSpool))
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewKill()); err != nil {
		t.Fatal(err)
	}
	exit := readExit(t, conn)
	if exit.Termination.Outcome != "UserKilled" {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestRunnerEmitsPeriodicHeartbeat(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runner.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-heartbeat",
			SocketPath:    socket,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "sleep 30"},
			Stdout:        &bytes.Buffer{},
			Stderr:        &bytes.Buffer{},
		})
	}()

	conn := dialRunner(t, socket, errs)
	defer conn.Close()
	if _, err := runnerproto.ReadPreamble(conn); err != nil {
		t.Fatal(err)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewSubscribe(true, 1)); err != nil {
		t.Fatal(err)
	}
	first := readHeartbeat(t, conn)
	second := readHeartbeat(t, conn)
	if first.RunID != "run-heartbeat" || second.RunID != "run-heartbeat" {
		t.Fatalf("heartbeats = %+v %+v", first, second)
	}
	if err := runnerproto.WriteMessage(conn, runnerproto.NewKill()); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerReplayReadsFromSpoolIndex(t *testing.T) {
	dir := t.TempDir()
	spool := filepath.Join(dir, "runner.out")
	if err := os.WriteFile(spool, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	s := &server{
		cfg: Config{SpoolPath: spool, Stderr: &bytes.Buffer{}},
		outputIndex: []outputIndexEntry{
			{seq: 1, stream: "Stdout", offset: 0, size: 6},
			{seq: 2, stream: "Stdout", offset: 6, size: 7},
		},
	}
	done := make(chan struct{})
	go func() {
		s.replay(&client{conn: serverConn}, 2)
		_ = serverConn.Close()
		close(done)
	}()
	chunk := readOutputChunk(t, clientConn)
	if chunk.ChunkSeq != 2 || chunk.Stream != "Stdout" || string(chunk.Bytes) != "second\n" {
		t.Fatalf("chunk = %+v", chunk)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("replay did not finish")
	}
}

func TestRunnerPassesEnvFileToChildAndRemovesFile(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "runner.sock")
	envFile := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(envFile, []byte("AGENCY_TEST_ENV=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Serve(ctx, Config{
			RunID:         "run-env",
			SocketPath:    socket,
			EnvFile:       envFile,
			BinaryVersion: "test",
			Command:       []string{"/bin/sh", "-c", "printf 'env:%s\\n' \"$AGENCY_TEST_ENV\""},
			Stdout:        &output,
			Stderr:        &bytes.Buffer{},
		})
	}()

	eventually(t, func() bool {
		return bytes.Contains(output.Bytes(), []byte("env:from-file"))
	})
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("env file was not removed: %v", err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit")
	}
}

func TestRunParsesDefaultSocketFromRuntimeDir(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	cfg, err := parseArgs([]string{"-run-id", "run-default", "/bin/true"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(runtimeDir, "agency", "runners", "run-default.sock")
	if cfg.SocketPath != want {
		t.Fatalf("socket path = %q, want %q", cfg.SocketPath, want)
	}
	if cfg.Command[0] != "/bin/true" {
		t.Fatalf("command = %#v", cfg.Command)
	}
}

func readHeartbeat(t *testing.T, conn net.Conn) runnerproto.Heartbeat {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		if heartbeat, ok := msg.(runnerproto.Heartbeat); ok {
			return heartbeat
		}
	}
	t.Fatal("timed out waiting for heartbeat")
	return runnerproto.Heartbeat{}
}

func readAck(t *testing.T, conn net.Conn, seq int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		if ack, ok := msg.(runnerproto.Ack); ok && ack.Seq == seq {
			return
		}
	}
	t.Fatalf("timed out waiting for ack %d", seq)
}

func readOutputChunk(t *testing.T, conn net.Conn) runnerproto.OutputChunk {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		if chunk, ok := msg.(runnerproto.OutputChunk); ok {
			return chunk
		}
	}
	t.Fatal("timed out waiting for output chunk")
	return runnerproto.OutputChunk{}
}

func dialRunner(t *testing.T, socket string, errs <-chan error) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-errs:
			t.Fatalf("runner exited before dial: %v", err)
		default:
		}
		conn, err := net.Dial("unix", socket)
		if err == nil {
			return conn
		}
		lastErr = err
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial runner: %v", lastErr)
	return nil
}

func readExit(t *testing.T, conn net.Conn) runnerproto.Exit {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		msg, err := runnerproto.ReadMessage(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		if exit, ok := msg.(runnerproto.Exit); ok {
			return exit
		}
	}
	t.Fatal("timed out waiting for exit")
	return runnerproto.Exit{}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}
