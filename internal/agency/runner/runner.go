package runner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"agency-two/internal/agency/runnerproto"
)

const BinaryVersion = "dev"

// defaultHeartbeatInterval is the fallback runner→supervisor heartbeat cadence
// used when the supervisor does not pass -heartbeat-interval-ms. The supervisor
// owns the effective value from [timing].heartbeat_interval_ms and must keep it
// well under heartbeat_ttl_ms so a single missed beat never trips liveness.
const defaultHeartbeatInterval = 2 * time.Second

// outputDrainGrace bounds how long run() waits for the PTY reader to finish
// draining after the child exits before it announces the terminal outcome.
const outputDrainGrace = 2 * time.Second

type Config struct {
	RunID         string
	SocketPath    string
	SpoolPath     string
	BinaryVersion string
	Command       []string
	Env           []string
	EnvFile       string
	Dir           string
	// HeartbeatInterval is the config-owned heartbeat cadence supplied by the
	// supervisor (-heartbeat-interval-ms). Zero selects defaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	Stdout            io.Writer
	Stderr            io.Writer
	// Stdin, when set, is forwarded to the child terminal so a user attached to
	// the tmux pane can interact with the provider directly (spec: attach opens a
	// session for direct interaction). In production this is the pane's os.Stdin;
	// it is nil in tests and headless contexts, which disables forwarding.
	Stdin io.Reader
}

func Run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// The runner runs as the tmux pane's process, so its stdin is the pane: forward
	// it to the child so an attached user can type to the provider directly.
	cfg.Stdin = os.Stdin
	if err := Serve(context.Background(), cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func Serve(ctx context.Context, cfg Config) error {
	if cfg.RunID == "" {
		return errors.New("run id is required")
	}
	if cfg.SocketPath == "" {
		return errors.New("socket path is required")
	}
	if len(cfg.Command) == 0 {
		return errors.New("child command is required")
	}
	if cfg.BinaryVersion == "" {
		cfg.BinaryVersion = BinaryVersion
	}
	if cfg.SpoolPath == "" {
		cfg.SpoolPath = cfg.SocketPath + ".output"
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0700); err != nil {
		return err
	}
	_ = os.Remove(cfg.SocketPath)
	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(cfg.SocketPath)
	if err := os.Chmod(cfg.SocketPath, 0600); err != nil {
		return err
	}

	if cfg.EnvFile != "" {
		env, err := loadEnvFile(cfg.EnvFile)
		if err != nil {
			return err
		}
		cfg.Env = append(cfg.Env, env...)
	}

	child, err := startChild(cfg)
	if err != nil {
		return err
	}
	defer child.closePTY()
	spool, err := os.OpenFile(cfg.SpoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer spool.Close()

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	exited := make(chan runnerproto.Termination, 1)
	server := &server{
		cfg:          cfg,
		listener:     ln,
		child:        child,
		spool:        spool,
		exited:       exited,
		done:         serveCtx.Done(),
		captureDone:  make(chan struct{}),
		clients:      map[*client]struct{}{},
		inputSeqs:    map[int64]struct{}{},
		nextChunkSeq: 1,
	}
	// captureOutput touches s.clients/s.nextChunkSeq under s.mu, so those fields
	// must be initialized before it starts (above), not inside run().
	go server.captureOutput()
	go func() {
		// The channel is buffered, so this send never blocks even if run() has
		// already returned; do not cancel here — letting run() observe the exit
		// deterministically is what guarantees Exit is broadcast (a cancel would
		// race the exit against the done branch and could drop the terminal
		// outcome).
		exited <- child.wait()
	}()
	return server.run()
}

type childProcess struct {
	cmd *exec.Cmd
	pty *os.File

	mu        sync.Mutex
	requested terminationIntent
}

// terminationIntent records whether the supervisor asked this child to stop or
// be killed, so wait() can attribute the exit to the user even when the provider
// responds to SIGTERM by exiting cleanly (exit 0) rather than dying by signal.
type terminationIntent int

const (
	intentNone terminationIntent = iota
	intentStopped
	intentKilled
)

func startChild(cfg Config) (*childProcess, error) {
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	ptmx, tty, err := openPTY()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	// Size the child terminal before start: a freshly opened PTY defaults to 0x0,
	// which makes full-screen provider TUIs (claude/codex) render garbled and
	// splits prompt text across wrapped lines, defeating prompt-state detection.
	// Match the runner's own controlling terminal (the tmux pane) when it is a
	// TTY, else a standard 80x24.
	setInitialWinsize(ptmx, cfg.Stdout)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, err
	}
	return &childProcess{cmd: cmd, pty: ptmx}, nil
}

func openPTY() (*os.File, *os.File, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(masterFD)
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		unix.Close(masterFD)
		return nil, nil, err
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		unix.Close(masterFD)
		return nil, nil, err
	}
	return os.NewFile(uintptr(masterFD), "agency-runner-pty-master"), os.NewFile(uintptr(slaveFD), "agency-runner-pty-slave"), nil
}

// setInitialWinsize gives the child PTY a sane terminal size. It copies the size
// of the runner's own controlling terminal (the tmux pane, delivered as its
// stdout) when that is a TTY, so the provider renders at the pane's dimensions;
// otherwise it falls back to a standard 80x24. Best-effort: a failure leaves the
// kernel default rather than aborting the launch.
func setInitialWinsize(ptmx *os.File, stdout io.Writer) {
	rows, cols := uint16(24), uint16(80)
	if f, ok := stdout.(*os.File); ok {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Row > 0 && ws.Col > 0 {
			rows, cols = ws.Row, ws.Col
		}
	}
	_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

func (c *childProcess) writeInput(bytes []byte) error {
	_, err := c.pty.Write(bytes)
	return err
}

// forwardStdin copies the runner's stdin (the tmux pane, when attached) to the
// child terminal so a user's keystrokes reach the provider directly. It ends on
// EOF/read error (pane closed) or a child-write failure (child gone). It runs for
// the process lifetime; the runner exits shortly after the child, reaping it.
// Programmatic input continues to arrive via the socket SendInput path.
func (s *server) forwardStdin() {
	buf := make([]byte, 4096)
	for {
		n, readErr := s.cfg.Stdin.Read(buf)
		if n > 0 {
			if err := s.child.writeInput(buf[:n]); err != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (c *childProcess) stop() error {
	c.mu.Lock()
	if c.requested < intentStopped {
		c.requested = intentStopped
	}
	c.mu.Unlock()
	return c.signal(syscall.SIGTERM)
}

func (c *childProcess) kill() error {
	c.mu.Lock()
	c.requested = intentKilled
	c.mu.Unlock()
	return c.signal(syscall.SIGKILL)
}

func (c *childProcess) intent() terminationIntent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requested
}

func (c *childProcess) signal(sig syscall.Signal) error {
	if c.cmd.Process == nil {
		return errors.New("child process has not started")
	}
	err := syscall.Kill(-c.cmd.Process.Pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return err
	}
	return c.cmd.Process.Signal(sig)
}

func (c *childProcess) wait() runnerproto.Termination {
	err := c.cmd.Wait()
	intent := c.intent()
	if err == nil {
		// Clean exit (code 0). A provider that handles SIGTERM/SIGKILL by shutting
		// down gracefully exits 0, so attribute the exit to the user's request
		// when one was made rather than reporting a spontaneous provider exit.
		switch intent {
		case intentKilled:
			return runnerproto.UserKilled("SIGKILL")
		case intentStopped:
			return runnerproto.UserStopped()
		default:
			return runnerproto.ProviderExited(0)
		}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if ok {
			if status.Signaled() {
				switch status.Signal() {
				case syscall.SIGTERM, syscall.SIGINT:
					return runnerproto.UserStopped()
				case syscall.SIGKILL:
					return runnerproto.UserKilled("SIGKILL")
				default:
					return runnerproto.RunnerFailed("ChildSignaled", status.Signal().String())
				}
			}
			// Non-zero exit code: a provider often exits non-zero in response to a
			// stop/kill signal it caught, so honor the recorded intent first.
			switch intent {
			case intentKilled:
				return runnerproto.UserKilled("SIGKILL")
			case intentStopped:
				return runnerproto.UserStopped()
			default:
				return runnerproto.ProviderExited(status.ExitStatus())
			}
		}
	}
	return runnerproto.RunnerFailed("WaitFailed", err.Error())
}

func (c *childProcess) closePTY() {
	_ = c.pty.Close()
}

type server struct {
	cfg         Config
	listener    net.Listener
	child       *childProcess
	spool       *os.File
	exited      <-chan runnerproto.Termination
	done        <-chan struct{}
	captureDone chan struct{}

	mu           sync.Mutex
	clients      map[*client]struct{}
	outputIndex  []outputIndexEntry
	nextChunkSeq int64
	termination  *runnerproto.Termination
	inputSeqs    map[int64]struct{}
}

type outputIndexEntry struct {
	seq    int64
	stream string
	offset int64
	size   int64
}

type client struct {
	conn       net.Conn
	mu         sync.Mutex
	inputMu    sync.Mutex
	subscribed bool
}

func (s *server) run() error {
	defer s.closeClients()
	if s.cfg.Stdin != nil {
		go s.forwardStdin()
	}
	heartbeat := time.NewTicker(s.cfg.HeartbeatInterval)
	defer heartbeat.Stop()
	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			c := &client{conn: conn}
			s.addClient(c)
			go s.handle(c)
		}
	}()

	for {
		select {
		case termination := <-s.exited:
			s.setTermination(termination)
			// Drain the PTY before announcing Exit so trailing output chunks
			// reach subscribers ahead of the terminal frame; the supervisor
			// stops reading once it sees Exit and will not reconnect to a
			// terminal run, so any chunk emitted after Exit would be lost.
			s.drainOutput()
			s.broadcastExit(termination)
			return nil
		case err := <-acceptErr:
			select {
			case <-s.done:
				return nil
			default:
			}
			return err
		case <-heartbeat.C:
			s.broadcastHeartbeat()
		case <-s.done:
			return nil
		}
	}
}

// drainOutput waits for captureOutput to finish reading the PTY (which happens
// promptly once the child exits and the PTY reports EIO), bounded by a grace
// window so a wedged read can never hang terminal reporting.
func (s *server) drainOutput() {
	select {
	case <-s.captureDone:
	case <-time.After(outputDrainGrace):
	}
}

// closeClients closes every accepted connection so handle goroutines unblock
// and exit when the server stops.
func (s *server) closeClients() {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		_ = c.conn.Close()
	}
}

func (s *server) captureOutput() {
	defer close(s.captureDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.child.pty.Read(buf)
		if n > 0 {
			raw := append([]byte(nil), buf[:n]...)
			if _, writeErr := s.cfg.Stdout.Write(raw); writeErr != nil {
				fmt.Fprintf(s.cfg.Stderr, "write child output: %v\n", writeErr)
			}
			s.publishOutput(raw)
		}
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(s.cfg.Stderr, "copy child output: %v\n", err)
			}
			return
		}
	}
}

func (s *server) publishOutput(raw []byte) {
	s.mu.Lock()
	offset, err := s.spool.Seek(0, io.SeekEnd)
	if err != nil {
		fmt.Fprintf(s.cfg.Stderr, "seek output spool: %v\n", err)
		offset = -1
	}
	if _, err := s.spool.Write(raw); err != nil {
		fmt.Fprintf(s.cfg.Stderr, "write output spool: %v\n", err)
		offset = -1
	}
	seq := s.nextChunkSeq
	s.nextChunkSeq++
	if offset >= 0 {
		s.outputIndex = append(s.outputIndex, outputIndexEntry{seq: seq, stream: "Stdout", offset: offset, size: int64(len(raw))})
	}
	chunk := runnerproto.NewOutputChunk(seq, "Stdout", append([]byte(nil), raw...))
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		if c.isSubscribed() {
			clients = append(clients, c)
		}
	}
	s.mu.Unlock()
	for _, c := range clients {
		_ = c.write(chunk)
	}
}

func (s *server) handle(c *client) {
	defer s.removeClient(c)
	if err := c.writePreamble(runnerproto.NewPreamble(s.cfg.RunID, s.cfg.BinaryVersion)); err != nil {
		return
	}
	if termination := s.currentTermination(); termination != nil {
		_ = c.write(runnerproto.NewExit(*termination))
		return
	}
	for {
		msg, err := runnerproto.ReadMessage(c.conn)
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case runnerproto.Hello:
			if m.Version != runnerproto.ProtocolVersion {
				_ = c.write(runnerproto.NewNack(0, "IncompatibleProtocol", fmt.Sprintf("runner protocol %d does not accept hello version %d", runnerproto.ProtocolVersion, m.Version)))
				continue
			}
			_ = c.write(runnerproto.NewHeartbeat(s.cfg.RunID))
		case runnerproto.Subscribe:
			c.setSubscribed(m.Output)
			_ = c.write(runnerproto.NewHeartbeat(s.cfg.RunID))
			if m.Output {
				s.replay(c, m.FromChunkSeq)
			}
		case runnerproto.SendInput:
			if !s.claimInputSeq(m.Seq) {
				_ = c.write(runnerproto.NewAck(m.Seq))
				continue
			}
			s.writeInputResponse(c, m)
		case runnerproto.Stop:
			_ = s.child.stop()
		case runnerproto.Kill:
			_ = s.child.kill()
		}
	}
}

func (s *server) addClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = struct{}{}
}

func (s *server) removeClient(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
	_ = c.conn.Close()
}

func (s *server) setTermination(termination runnerproto.Termination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.termination = &termination
}

func (s *server) currentTermination() *runnerproto.Termination {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.termination == nil {
		return nil
	}
	termination := *s.termination
	return &termination
}

func (s *server) broadcastHeartbeat() {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	// Heartbeats go to every connected client, not just output subscribers:
	// liveness is independent of whether a client is tailing output.
	for _, c := range clients {
		_ = c.write(runnerproto.NewHeartbeat(s.cfg.RunID))
	}
}

func (s *server) broadcastExit(termination runnerproto.Termination) {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	msg := runnerproto.NewExit(termination)
	for _, c := range clients {
		c.inputMu.Lock()
		_ = c.write(msg)
		c.inputMu.Unlock()
	}
}

func (s *server) writeInputResponse(c *client, msg runnerproto.SendInput) {
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if err := s.child.writeInput(msg.Bytes); err != nil {
		s.releaseInputSeq(msg.Seq)
		_ = c.write(runnerproto.NewNack(msg.Seq, "PtyWriteFailed", err.Error()))
		return
	}
	_ = c.write(runnerproto.NewAck(msg.Seq))
}

func (s *server) replay(c *client, fromChunkSeq int64) {
	s.mu.Lock()
	entries := make([]outputIndexEntry, 0, len(s.outputIndex))
	for _, entry := range s.outputIndex {
		if entry.seq >= fromChunkSeq {
			entries = append(entries, entry)
		}
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	spool, err := os.Open(s.cfg.SpoolPath)
	if err != nil {
		_ = c.write(runnerproto.NewNack(0, "SpoolReplayFailed", err.Error()))
		return
	}
	defer spool.Close()
	for _, entry := range entries {
		raw := make([]byte, entry.size)
		if _, err := spool.ReadAt(raw, entry.offset); err != nil {
			_ = c.write(runnerproto.NewNack(0, "SpoolReplayFailed", err.Error()))
			return
		}
		_ = c.write(runnerproto.NewOutputChunk(entry.seq, entry.stream, raw))
	}
}

func (s *server) claimInputSeq(seq int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputSeqs == nil {
		s.inputSeqs = map[int64]struct{}{}
	}
	if _, ok := s.inputSeqs[seq]; ok {
		return false
	}
	s.inputSeqs[seq] = struct{}{}
	return true
}

func (s *server) releaseInputSeq(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inputSeqs, seq)
}

func (c *client) writePreamble(preamble runnerproto.Preamble) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return runnerproto.WritePreamble(c.conn, preamble)
}

func (c *client) write(msg any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return runnerproto.WriteMessage(c.conn, msg)
}

func (c *client) setSubscribed(subscribed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscribed = subscribed
}

func (c *client) isSubscribed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscribed
}

func parseArgs(args []string, stdout, stderr io.Writer) (Config, error) {
	var cfg Config
	cfg.Stdout = stdout
	cfg.Stderr = stderr
	cfg.BinaryVersion = BinaryVersion

	fs := flag.NewFlagSet("agency-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.RunID, "run-id", "", "agency run id")
	fs.StringVar(&cfg.SocketPath, "socket", "", "runner Unix socket path")
	fs.StringVar(&cfg.SpoolPath, "spool", "", "runner output spool path")
	fs.StringVar(&cfg.BinaryVersion, "runner-binary-version", BinaryVersion, "runner binary version")
	fs.StringVar(&cfg.Dir, "cwd", "", "child working directory")
	fs.StringVar(&cfg.EnvFile, "env-file", "", "file with child environment entries")
	var heartbeatIntervalMs int
	fs.IntVar(&heartbeatIntervalMs, "heartbeat-interval-ms", 0, "runner heartbeat cadence in milliseconds")
	fs.Func("env", "", func(value string) error {
		if err := validateEnv(value); err != nil {
			return err
		}
		cfg.Env = append(cfg.Env, value)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if heartbeatIntervalMs < 0 {
		return Config{}, errors.New("-heartbeat-interval-ms must not be negative")
	}
	cfg.HeartbeatInterval = time.Duration(heartbeatIntervalMs) * time.Millisecond
	cfg.Command = fs.Args()
	if len(cfg.Command) > 0 && cfg.Command[0] == "--" {
		cfg.Command = cfg.Command[1:]
	}
	if cfg.RunID == "" {
		return Config{}, errors.New("-run-id is required")
	}
	if cfg.SocketPath == "" {
		socket, err := defaultSocketPath(cfg.RunID)
		if err != nil {
			return Config{}, err
		}
		cfg.SocketPath = socket
	}
	if len(cfg.Command) == 0 {
		return Config{}, errors.New("child command is required")
	}
	return cfg, nil
}

func loadEnvFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	body := strings.TrimSuffix(string(raw), "\n")
	if body == "" {
		return nil, nil
	}
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if err := validateEnv(line); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func validateEnv(value string) error {
	// Never include the value in an error: it may carry a provider credential.
	name, _, ok := strings.Cut(value, "=")
	if !ok {
		return errors.New("invalid environment assignment: expected NAME=VALUE")
	}
	if name == "" {
		return errors.New("invalid environment assignment: empty variable name")
	}
	if strings.ContainsAny(value, "\x00\n\r") {
		return fmt.Errorf("invalid environment assignment for %q: contains control characters", name)
	}
	return nil
}

func defaultSocketPath(runID string) (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return "", errors.New("-socket is required when XDG_RUNTIME_DIR is unset")
	}
	if strings.ContainsRune(runID, filepath.Separator) {
		return "", errors.New("run id cannot contain path separators")
	}
	return filepath.Join(runtimeDir, "agency", "runners", runID+".sock"), nil
}
