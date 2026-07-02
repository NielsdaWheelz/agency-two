package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	defaultBinary = "tmux"
	runnerWindow  = "runner"
)

var (
	ErrTmuxMissing   = errors.New("tmux cli missing")
	ErrServerMissing = errors.New("tmux server missing")
	ErrTargetMissing = errors.New("tmux target missing")

	validSessionName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type Adapter struct {
	Binary     string
	SocketPath string
	SocketName string
	ConfigPath string
}

type Capability struct {
	Available  bool
	BinaryPath string
	Version    string
}

type TargetSpec struct {
	SessionName string
	SessionID   string
	WindowName  string
	WindowIndex int
	WindowID    string
	PaneIndex   int
	PaneID      string
}

type ServerIdentity struct {
	PID       int   `json:"pid"`
	StartedAt int64 `json:"startedAt"`
}

func New() Adapter {
	return Adapter{Binary: defaultBinary}
}

func NewWithSocketPath(socketPath string) Adapter {
	return Adapter{Binary: defaultBinary, SocketPath: socketPath}
}

func Probe(ctx context.Context) (Capability, error) {
	return New().Probe(ctx)
}

func (a Adapter) Probe(ctx context.Context) (Capability, error) {
	binary := a.binary()
	path, err := exec.LookPath(binary)
	if err != nil {
		return Capability{Available: false}, ErrTmuxMissing
	}

	out, err := exec.CommandContext(ctx, path, "-V").CombinedOutput()
	if err != nil {
		return Capability{Available: false, BinaryPath: path}, commandError("tmux -V", err, out)
	}
	version := strings.TrimSpace(string(out))
	version = strings.TrimPrefix(version, "tmux ")
	if version == "" {
		return Capability{Available: false, BinaryPath: path}, errors.New("tmux capability probe returned an empty version")
	}
	return Capability{Available: true, BinaryPath: path, Version: version}, nil
}

func (a Adapter) CreateRunnerTarget(ctx context.Context, sessionName, command string) (TargetSpec, error) {
	if err := validateSessionName(sessionName); err != nil {
		return TargetSpec{}, err
	}
	if strings.TrimSpace(command) == "" {
		return TargetSpec{}, errors.New("runner command is required")
	}
	if err := a.validate(); err != nil {
		return TargetSpec{}, err
	}

	format := strings.Join([]string{
		"#{session_name}",
		"#{session_id}",
		"#{window_name}",
		"#{window_index}",
		"#{window_id}",
		"#{pane_index}",
		"#{pane_id}",
	}, "\t")

	out, err := a.run(ctx,
		"new-session",
		"-d",
		"-P",
		"-F", format,
		"-s", sessionName,
		"-n", runnerWindow,
		"--", command,
	)
	if err != nil {
		return TargetSpec{}, commandError("tmux new-session", err, out)
	}

	return parseTargetSpec(out)
}

func (a Adapter) AttachCommand(sessionName string) ([]string, error) {
	if err := validateSessionName(sessionName); err != nil {
		return nil, err
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return a.argv("attach-session", "-t", sessionName), nil
}

func (a Adapter) TargetExists(ctx context.Context, sessionName string) (bool, error) {
	if err := validateSessionName(sessionName); err != nil {
		return false, err
	}
	if err := a.validate(); err != nil {
		return false, err
	}

	out, err := a.run(ctx, "has-session", "-t", sessionName)
	if err == nil {
		return true, nil
	}
	if outputMeansMissing(out) {
		return false, nil
	}
	return false, commandError("tmux has-session", err, out)
}

func (a Adapter) ServerIdentity(ctx context.Context) (ServerIdentity, error) {
	if err := a.validate(); err != nil {
		return ServerIdentity{}, err
	}

	out, err := a.run(ctx, "display-message", "-p", "#{pid}\t#{start_time}")
	if err != nil {
		if outputMeansMissing(out) {
			return ServerIdentity{}, fmt.Errorf("%w: %s", ErrServerMissing, cleanOutput(out))
		}
		return ServerIdentity{}, commandError("tmux display-message", err, out)
	}

	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 2 {
		return ServerIdentity{}, fmt.Errorf("unexpected tmux server identity output %q", strings.TrimSpace(string(out)))
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return ServerIdentity{}, fmt.Errorf("parse tmux server pid %q: %w", fields[0], err)
	}
	startedAt, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return ServerIdentity{}, fmt.Errorf("parse tmux server start time %q: %w", fields[1], err)
	}
	return ServerIdentity{PID: pid, StartedAt: startedAt}, nil
}

func (a Adapter) KillTarget(ctx context.Context, sessionName string) error {
	if err := validateSessionName(sessionName); err != nil {
		return err
	}
	if err := a.validate(); err != nil {
		return err
	}

	out, err := a.run(ctx, "kill-session", "-t", sessionName)
	if err == nil {
		return nil
	}
	if outputMeansMissing(out) {
		return fmt.Errorf("%w: %s", ErrTargetMissing, cleanOutput(out))
	}
	return commandError("tmux kill-session", err, out)
}

func (a Adapter) binary() string {
	if a.Binary == "" {
		return defaultBinary
	}
	return a.Binary
}

func (a Adapter) validate() error {
	if a.SocketPath != "" && a.SocketName != "" {
		return errors.New("tmux adapter accepts SocketPath or SocketName, not both")
	}
	return nil
}

func (a Adapter) argv(args ...string) []string {
	argv := []string{a.binary()}
	if a.ConfigPath != "" {
		argv = append(argv, "-f", a.ConfigPath)
	}
	if a.SocketPath != "" {
		argv = append(argv, "-S", a.SocketPath)
	}
	if a.SocketName != "" {
		argv = append(argv, "-L", a.SocketName)
	}
	argv = append(argv, args...)
	return argv
}

func (a Adapter) run(ctx context.Context, args ...string) ([]byte, error) {
	argv := a.argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	return cmd.CombinedOutput()
}

func validateSessionName(sessionName string) error {
	if sessionName == "" {
		return errors.New("tmux session name is required")
	}
	if !validSessionName.MatchString(sessionName) {
		return fmt.Errorf("invalid tmux session name %q: use letters, digits, dot, underscore, or dash", sessionName)
	}
	return nil
}

func parseTargetSpec(out []byte) (TargetSpec, error) {
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 7 {
		return TargetSpec{}, fmt.Errorf("unexpected tmux target output %q", strings.TrimSpace(string(out)))
	}
	windowIndex, err := strconv.Atoi(fields[3])
	if err != nil {
		return TargetSpec{}, fmt.Errorf("parse tmux window index %q: %w", fields[3], err)
	}
	paneIndex, err := strconv.Atoi(fields[5])
	if err != nil {
		return TargetSpec{}, fmt.Errorf("parse tmux pane index %q: %w", fields[5], err)
	}
	return TargetSpec{
		SessionName: fields[0],
		SessionID:   fields[1],
		WindowName:  fields[2],
		WindowIndex: windowIndex,
		WindowID:    fields[4],
		PaneIndex:   paneIndex,
		PaneID:      fields[6],
	}, nil
}

func commandError(action string, err error, out []byte) error {
	msg := cleanOutput(out)
	if msg == "" {
		return fmt.Errorf("%s failed: %w", action, err)
	}
	return fmt.Errorf("%s failed: %w: %s", action, err, msg)
}

func outputMeansMissing(out []byte) bool {
	msg := cleanOutput(out)
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "server exited unexpectedly") ||
		(strings.Contains(msg, "error connecting to") && strings.Contains(msg, "No such file or directory"))
}

func cleanOutput(out []byte) string {
	return strings.TrimSpace(string(out))
}
