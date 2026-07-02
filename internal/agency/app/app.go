package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agency-two/internal/agency/content"
	"agency-two/internal/agency/eventlog"
	"agency-two/internal/agency/gitx"
	"agency-two/internal/agency/provider"
	"agency-two/internal/agency/remote"
	"agency-two/internal/agency/storage"
	"agency-two/internal/agency/supervisor"
	"agency-two/internal/agency/tui"
)

func Run(args []string, stdout, stderr io.Writer) int {
	global, args, err := parseGlobalFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("parse command", "Invalid command input", err.Error(), "Global flag is invalid.", "Run agency --host <host> <command>."))
		return 2
	}
	args = applyGlobalFlags(args, global)
	if global.Host != "" && global.Host != "local" {
		return runRemote(global.Host, args, stdout, stderr)
	}
	if len(args) == 0 {
		return runTUI(stdout, stderr)
	}
	switch args[0] {
	case "new":
		return runNew(args[1:], stdout, stderr)
	case "run":
		return runAdditional(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "events":
		return runEvents(args[1:], stdout, stderr)
	case "diff":
		return runDiff(args[1:], stdout, stderr)
	case "attach":
		return runAttach(args[1:], stdout, stderr)
	case "send":
		return runSend(args[1:], stdout, stderr)
	case "rename":
		return runRename(args[1:], stdout, stderr)
	case "stop":
		return runStop(args[1:], stdout, stderr)
	case "kill":
		return runKill(args[1:], stdout, stderr)
	case "close":
		return runClose(args[1:], stdout, stderr)
	case "model":
		return runModel(args[1:], stdout, stderr)
	case "profile":
		return runProfile(args[1:], stdout, stderr)
	case "project":
		return runProject(args[1:], stdout, stderr)
	case "host":
		return runHost(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "worktree", "repair", "prune", "export", "config":
		if args[0] == "worktree" {
			return runWorktree(args[1:], stdout, stderr)
		}
		if args[0] == "config" {
			return runConfig(args[1:], stdout, stderr)
		}
		if args[0] == "prune" {
			return runPrune(args[1:], stdout, stderr)
		}
		if args[0] == "export" {
			return runExport(args[1:], stdout, stderr)
		}
		return runRepair(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, content.ErrorText("parse command", "Invalid command input", args[0], "Unknown command.", "Run agency."))
		return 2
	}
}

type globalFlags struct {
	Host      string
	Project   string
	Profile   string
	ReplayKey string
	JSON      bool
	NoColor   bool
}

func parseGlobalFlags(args []string) (globalFlags, []string, error) {
	var flags globalFlags
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if arg == "--json" {
			flags.JSON = true
			continue
		}
		if arg == "--no-color" {
			flags.NoColor = true
			continue
		}
		if key, value, ok := strings.Cut(arg, "="); ok && isGlobalStringFlag(key) {
			if value == "" {
				return globalFlags{}, nil, errors.New(key + " value is required")
			}
			setGlobalStringFlag(&flags, key, value)
			continue
		}
		if isGlobalStringFlag(arg) {
			if i+1 >= len(args) || args[i+1] == "" {
				return globalFlags{}, nil, errors.New(arg + " value is required")
			}
			setGlobalStringFlag(&flags, arg, args[i+1])
			i++
			continue
		}
		rest = append(rest, arg)
	}
	return flags, rest, nil
}

func isGlobalStringFlag(flag string) bool {
	return flag == "--host" || flag == "--project" || flag == "--profile" || flag == "--replay-key"
}

func setGlobalStringFlag(flags *globalFlags, flag, value string) {
	switch flag {
	case "--host":
		flags.Host = value
	case "--project":
		flags.Project = value
	case "--profile":
		flags.Profile = value
	case "--replay-key":
		flags.ReplayKey = value
	}
}

func applyGlobalFlags(args []string, global globalFlags) []string {
	if len(args) == 0 {
		return args
	}
	out := append([]string(nil), args...)
	if global.JSON {
		out = insertGlobalFlag(out, "--json", "")
	}
	if global.Project != "" {
		out = insertGlobalProject(out, global.Project)
	}
	if global.Profile != "" {
		out = insertGlobalFlag(out, "--profile", global.Profile)
	}
	if global.ReplayKey != "" {
		out = insertGlobalFlag(out, "--replay-key", global.ReplayKey)
	}
	return out
}

func insertGlobalProject(args []string, value string) []string {
	if len(args) > 0 && args[0] == "new" {
		return insertGlobalFlag(args, "--cwd", value)
	}
	return insertGlobalFlag(args, "--project", value)
}

func insertGlobalFlag(args []string, flag, value string) []string {
	index := globalFlagIndex(args, flag)
	if index < 0 {
		return args
	}
	added := []string{flag}
	if value != "" {
		added = append(added, value)
	}
	out := make([]string, 0, len(args)+len(added))
	out = append(out, args[:index]...)
	out = append(out, added...)
	out = append(out, args[index:]...)
	return out
}

func globalFlagIndex(args []string, flag string) int {
	switch args[0] {
	case "new":
		if flag == "--json" || flag == "--cwd" || flag == "--profile" || flag == "--replay-key" {
			if len(args) >= 2 {
				return 2
			}
		}
	case "run", "send", "status", "stop", "close", "rename", "repair":
		if flag == "--json" && args[0] != "status" && args[0] != "repair" {
			return -1
		}
		if flag == "--project" || flag == "--profile" {
			return -1
		}
		return 1
	case "list", "events", "doctor":
		if flag == "--json" {
			return 1
		}
	case "model", "profile", "project", "worktree", "host", "config":
		if len(args) < 2 {
			return -1
		}
		if flag == "--json" {
			switch args[0] + " " + args[1] {
			case "model list", "profile list", "project init", "project list", "worktree list", "worktree status", "worktree close", "host list":
				return 2
			}
		}
		if flag == "--project" {
			switch args[0] + " " + args[1] {
			case "project init", "worktree list":
				return 2
			}
		}
		if flag == "--replay-key" {
			switch args[0] + " " + args[1] {
			case "project init", "config set", "worktree close":
				return 2
			}
		}
	}
	return -1
}

func runRemote(host string, args []string, stdout, stderr io.Writer) int {
	if !storage.ValidHostKey(host) {
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "InvalidHostKey", host, "Host key is not valid.", "Run agency host list."))
		return 2
	}
	store, err := storage.Open(context.Background(), defaultStatePath())
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "Storage error", defaultStatePath(), err.Error(), "Run agency doctor."))
		return 1
	}
	defer store.Close()
	record, err := store.Host(context.Background(), host)
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "HostNotFound", host, err.Error(), "Run agency config set hosts."+host+".access ssh:<alias>."))
		return 1
	}
	switch record.Access.Mode {
	case "Ssh":
		return runRemoteCommand("ssh", []string{record.Access.HostAlias, provider.ShellPreview(append([]string{"agency"}, args...))}, stdout, stderr)
	case "AttachOnlyMosh":
		if len(args) != 2 || args[0] != "attach" {
			fmt.Fprintln(stderr, content.ErrorText("run remote command", "AttachOnlyMosh", host, "Mosh hosts support attach only.", "Use --host <host> attach <session>."))
			return 1
		}
		if !validSessionHandle(args[1]) {
			fmt.Fprintln(stderr, content.ErrorText("run remote command", "InvalidSessionHandle", args[1], "Session handle is not valid.", "Run agency list."))
			return 2
		}
		return runRemoteCommand("mosh", []string{record.Access.HostAlias, "--", "tmux", "attach-session", "-t", "agency-" + args[1]}, stdout, stderr)
	default:
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "InvalidHostAccess", host, "Host access mode cannot run remote commands.", "Run agency host list."))
		return 1
	}
}

func validSessionHandle(value string) bool {
	if !strings.HasPrefix(value, "ses_") {
		return false
	}
	for _, r := range strings.TrimPrefix(value, "ses_") {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return len(value) > len("ses_")
}

func runRemoteCommand(name string, args []string, stdout, stderr io.Writer) int {
	if _, err := execLookPath(name); err != nil {
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "RemoteCommandMissing", name, err.Error(), "Install "+name+" or use host local."))
		return 4
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, content.ErrorText("run remote command", "RemoteCommandFailed", name, err.Error(), "Check SSH/mosh access and remote agency installation."))
		return 1
	}
	return 0
}

func runTUI(stdout, stderr io.Writer) int {
	socket := defaultSupervisorSocket()
	health, err := supervisor.HealthCheck(context.Background(), socket)
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("open TUI", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
		return 4
	}
	if health.Status == "" {
		return 1
	}
	if !realStdio(stdout, stderr) {
		sessions, err := supervisor.ListSessions(context.Background(), socket)
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("open TUI", "Supervisor API error", socket, err.Error(), "Run agency doctor --supervisor."))
			return 1
		}
		tui.RenderDashboard(stdout, tui.Dashboard{Host: "local", Sessions: sessions})
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	input := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input <- scanner.Text()
		}
		close(input)
	}()
	state := tuiState{view: "dashboard"}
	render := func() int {
		fmt.Fprint(stdout, "\x1b[H\x1b[2J")
		switch state.view {
		case "dashboard":
			sessions, err := supervisor.ListSessions(ctx, socket)
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("open TUI", "Supervisor API error", socket, err.Error(), "Run agency doctor --supervisor."))
				return 1
			}
			tui.RenderDashboard(stdout, tui.Dashboard{Host: "local", Sessions: sessions})
		case "session":
			if state.selectedSession == "" {
				fmt.Fprintln(stdout, "No session selected.")
				break
			}
			status, err := supervisor.SessionStatus(ctx, socket, state.selectedSession)
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("open TUI", "Supervisor API error", state.selectedSession, err.Error(), "Run agency doctor --supervisor."))
				return 1
			}
			tui.RenderSession(stdout, status)
		case "worktrees":
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("open TUI", "InvalidProjectRoot", ".", err.Error(), "Run agency project init."))
				return 1
			}
			worktrees, err := supervisor.ListWorktrees(ctx, socket, cwd)
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("open TUI", "Supervisor API error", "worktrees", err.Error(), "Run agency doctor --supervisor."))
				return 1
			}
			tui.RenderWorktrees(stdout, worktrees)
		case "doctor":
			report, err := supervisor.Doctor(ctx, socket)
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("open TUI", "Supervisor API error", "doctor", err.Error(), "Run agency doctor --supervisor."))
				return 1
			}
			tui.RenderDoctor(stdout, report)
		}
		if state.notice != "" {
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, state.notice)
		}
		tui.RenderFooter(stdout)
		return 0
	}
	if code := render(); code != 0 {
		return code
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case line, ok := <-input:
			if !ok {
				return 0
			}
			action := dispatchTUIAction(ctx, socket, line, &state)
			if len(action.attachArgv) > 0 {
				path, err := exec.LookPath(action.attachArgv[0])
				if err != nil {
					fmt.Fprintln(stderr, content.ErrorText("attach session", "TmuxAttachFailed", action.attachArgv[0], err.Error(), "Run agency status."))
					return 1
				}
				if err := syscall.Exec(path, action.attachArgv, os.Environ()); err != nil {
					fmt.Fprintln(stderr, content.ErrorText("attach session", "TmuxAttachFailed", strings.Join(action.attachArgv, " "), err.Error(), "Run agency status."))
					return 1
				}
			}
			if action.quit {
				return 0
			}
			if code := render(); code != 0 {
				return code
			}
		case <-ticker.C:
			if code := render(); code != 0 {
				return code
			}
		}
	}
}

type tuiState struct {
	view            string
	selectedSession string
	notice          string
}

type tuiActionResult struct {
	quit       bool
	attachArgv []string
}

func dispatchTUIAction(ctx context.Context, socket, line string, state *tuiState) tuiActionResult {
	line = strings.TrimSpace(line)
	state.notice = ""
	switch {
	case line == "q":
		return tuiActionResult{quit: true}
	case line == "" || line == "r":
		return tuiActionResult{}
	case line == "b":
		state.view = "dashboard"
		return tuiActionResult{}
	case line == "w":
		state.view = "worktrees"
		return tuiActionResult{}
	case line == "d":
		state.view = "doctor"
		return tuiActionResult{}
	case strings.HasPrefix(line, "s "):
		state.selectedSession = strings.TrimSpace(strings.TrimPrefix(line, "s "))
		state.view = "session"
		return tuiActionResult{}
	}

	args := strings.Fields(line)
	if len(args) == 0 {
		return tuiActionResult{}
	}
	switch args[0] {
	case "new":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runNew(args[1:], out, errOut) })
	case "run":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runAdditional(args[1:], out, errOut) })
	case "attach", "open":
		if len(args) != 2 {
			state.notice = "Could not attach session.\nreason: Invalid command input\ntarget: " + args[0] + "\ndetail: Session handle is required.\nnext: Type attach <session>."
			return tuiActionResult{}
		}
		result, err := supervisor.AttachCommand(ctx, socket, args[1])
		if err != nil {
			state.notice = content.ErrorText("attach session", "SessionNotFound", args[1], err.Error(), "Run agency list.")
			return tuiActionResult{}
		}
		return tuiActionResult{attachArgv: result.Argv}
	case "stop":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runStop(args[1:], out, errOut) })
	case "close":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runClose(args[1:], out, errOut) })
	case "send":
		if len(args) < 3 {
			state.notice = "Could not send input.\nreason: Invalid command input\ntarget: send\ndetail: Message is required in the TUI command line.\nnext: Type send <session> <message>."
			return tuiActionResult{}
		}
		recordTUICommand(state, func(out, errOut io.Writer) int { return runSend(args[1:], out, errOut) })
	case "worktree":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runWorktree(args[1:], out, errOut) })
	case "status":
		if len(args) != 2 {
			state.notice = "Could not open session.\nreason: Invalid command input\ntarget: status\ndetail: Session handle is required.\nnext: Type status <session>."
			return tuiActionResult{}
		}
		state.selectedSession = args[1]
		state.view = "session"
	case "diff":
		recordTUICommand(state, func(out, errOut io.Writer) int { return runDiff(args[1:], out, errOut) })
	case "doctor":
		if len(args) == 1 {
			state.view = "doctor"
			return tuiActionResult{}
		}
		recordTUICommand(state, func(out, errOut io.Writer) int { return runDoctor(args[1:], out, errOut) })
	default:
		state.notice = content.ErrorText("run TUI action", "Invalid command input", args[0], "Unknown TUI action.", "Use new, open, attach, stop, close, send, worktree, status, diff, doctor, or q.")
	}
	return tuiActionResult{}
}

func recordTUICommand(state *tuiState, run func(io.Writer, io.Writer) int) {
	var out, errOut bytes.Buffer
	_ = run(&out, &errOut)
	parts := []string{}
	if strings.TrimSpace(out.String()) != "" {
		parts = append(parts, strings.TrimSpace(out.String()))
	}
	if strings.TrimSpace(errOut.String()) != "" {
		parts = append(parts, strings.TrimSpace(errOut.String()))
	}
	state.notice = strings.Join(parts, "\n")
}

func RunSupervisor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agency-supervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var stateDB, socketPath string
	fs.StringVar(&stateDB, "state-db", defaultStatePath(), "")
	fs.StringVar(&socketPath, "socket", defaultSupervisorSocket(), "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintln(stdout, "Agency supervisor")
	fmt.Fprintln(stdout, "socket:", socketPath)
	if err := supervisor.Serve(context.Background(), supervisor.Config{StateDB: stateDB, SocketPath: socketPath}); err != nil {
		fmt.Fprintln(stderr, content.ErrorText("start supervisor", "SupervisorAlreadyRunning", socketPath, err.Error(), "Connect to the existing supervisor or stop it."))
		return 1
	}
	return 0
}

func runProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse project command", "Invalid command input", "project", "Missing project subcommand.", "Run agency project init or agency project list."))
		return 2
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("project init", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		var cwd, replayKey string
		fs.BoolVar(&jsonOut, "json", false, "")
		fs.StringVar(&cwd, "project", "", "")
		fs.StringVar(&replayKey, "replay-key", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, content.ErrorText("parse project command", "Invalid command input", "project init", "Too many arguments.", "Run agency project init."))
			return 2
		}
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("initialize project", "InvalidProjectRoot", ".", err.Error(), "Run agency project init from a git repository."))
				return 1
			}
		}
		if replayKey == "" {
			replayKey = derivedReplayKey("initProject", supervisor.ProjectParams{CWD: cwd})
		}
		socket := defaultSupervisorSocket()
		project, err := supervisor.InitProject(context.Background(), socket, cwd, replayKey)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("initialize project", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("initialize project", "InvalidProjectRoot", cwd, err.Error(), "Run agency project init from a git repository."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, project)
			return 0
		}
		fmt.Fprintln(stdout, "Project initialized")
		fmt.Fprintln(stdout, "project:", project.Project)
		fmt.Fprintln(stdout, "root:", project.RootPath)
		fmt.Fprintln(stdout, "managed worktree root:", project.ManagedWorktreeRoot)
		return 0
	case "list":
		fs := flag.NewFlagSet("project list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		fs.BoolVar(&jsonOut, "json", false, "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, content.ErrorText("parse project command", "Invalid command input", "project list", "Too many arguments.", "Run agency project list."))
			return 2
		}
		socket := defaultSupervisorSocket()
		projects, err := supervisor.ListProjects(context.Background(), socket)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("list projects", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("list projects", "Supervisor API error", "projects", err.Error(), "Run agency doctor --supervisor."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, projects)
			return 0
		}
		if len(projects) == 0 {
			fmt.Fprintln(stdout, "No projects are configured.")
			return 0
		}
		fmt.Fprintln(stdout, "PROJECT      ROOT")
		for _, p := range projects {
			fmt.Fprintf(stdout, "%-12s %s\n", p.Project, p.RootPath)
		}
		return 0
	default:
		fmt.Fprintln(stderr, content.ErrorText("parse project command", "Invalid command input", args[0], "Unknown project subcommand.", "Run agency project init or agency project list."))
		return 2
	}
}

func runModel(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(stderr, content.ErrorText("parse model command", "Invalid command input", "model", "Expected model list.", "Run agency model list."))
		return 2
	}
	fs := flag.NewFlagSet("model list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse model command", "Invalid command input", "model list", "Too many arguments.", "Run agency model list."))
		return 2
	}
	providerKey := ""
	if fs.NArg() == 1 {
		providerKey = fs.Arg(0)
	}
	socket := defaultSupervisorSocket()
	models, err := supervisor.ListModels(context.Background(), socket, providerKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("list models", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("list models", "Supervisor API error", providerKey, err.Error(), "Run agency doctor --supervisor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, models)
		return 0
	}
	fmt.Fprintln(stdout, "PROVIDER  MODEL                 EFFORTS                         AVAILABILITY")
	for _, m := range models {
		fmt.Fprintf(stdout, "%-8s  %-20s  %-30s  %s\n", m.Provider, m.Key, strings.Join(m.Efforts, ","), m.Availability)
	}
	return 0
}

func runProfile(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", "profile", "Missing profile subcommand.", "Run agency profile list."))
		return 2
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("profile list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		fs.BoolVar(&jsonOut, "json", false, "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", "profile list", "Too many arguments.", "Run agency profile list."))
			return 2
		}
		socket := defaultSupervisorSocket()
		profiles, err := supervisor.ListProfiles(context.Background(), socket)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("list profiles", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("list profiles", "Supervisor API error", "profiles", err.Error(), "Run agency doctor --supervisor."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, profiles)
			return 0
		}
		if len(profiles) == 0 {
			fmt.Fprintln(stdout, "No agent profiles are configured.")
			fmt.Fprintln(stdout, "Create one with agency profile create <name> --provider codex.")
			return 0
		}
		fmt.Fprintln(stdout, "PROFILE          PROVIDER  MODEL                 EFFORT  CONTROLS")
		for _, p := range profiles {
			controls := []string{}
			if p.PermissionMode != "" {
				controls = append(controls, "permission-mode="+p.PermissionMode)
			}
			if p.SandboxMode != "" {
				controls = append(controls, "sandbox="+p.SandboxMode)
			}
			if p.ApprovalPolicy != "" {
				controls = append(controls, "approval-policy="+p.ApprovalPolicy)
			}
			fmt.Fprintf(stdout, "%-16s %-8s  %-20s  %-6s  %s\n", p.Key, p.Provider, p.Model, p.Effort, strings.Join(controls, " "))
		}
		return 0
	case "create", "set", "set-default":
		return runProfileMutation(args, stdout, stderr)
	default:
		fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", args[0], "Unknown profile subcommand.", "Run agency profile list."))
		return 2
	}
}

func runProfileMutation(args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "create", "set":
		if len(args) < 2 {
			fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", "profile "+args[0], "Profile name is required.", "Run agency profile create <name> --provider codex."))
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("profile "+args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		var providerKey, model, effort, permissionMode, sandboxMode, approvalPolicy string
		fs.StringVar(&providerKey, "provider", "", "")
		fs.StringVar(&model, "model", "", "")
		fs.StringVar(&effort, "effort", "", "")
		fs.StringVar(&permissionMode, "permission-mode", "", "")
		fs.StringVar(&sandboxMode, "sandbox", "", "")
		fs.StringVar(&approvalPolicy, "approval-policy", "", "")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if providerKey == "" && args[0] == "create" {
			fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", name, "--provider is required.", "Run agency profile create <name> --provider codex."))
			return 2
		}
		if providerKey != "" {
			_, err := provider.BuildLaunchPlan(provider.LaunchInput{ProviderKey: providerKey, Model: model, Effort: effort, PermissionMode: permissionMode, SandboxMode: sandboxMode, ApprovalPolicy: approvalPolicy})
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("change profile", reasonFromProviderError(err), name, err.Error(), "Choose a supported provider control."))
				return 2
			}
		}
		socket := defaultSupervisorSocket()
		result, err := supervisor.SaveProfile(context.Background(), socket, supervisor.ProfileSaveParams{
			Profile: name, Provider: providerKey, Model: model, Effort: effort,
			PermissionMode: permissionMode, SandboxMode: sandboxMode, ApprovalPolicy: approvalPolicy,
		})
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("change profile", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("change profile", "Storage error", name, err.Error(), "Run agency profile list."))
			return 1
		}
		fmt.Fprintln(stdout, "Agent profile saved")
		fmt.Fprintln(stdout, "profile:", result.Profile)
		fmt.Fprintln(stdout, "provider:", result.Provider)
		return 0
	case "set-default":
		if len(args) != 2 {
			fmt.Fprintln(stderr, content.ErrorText("parse profile command", "Invalid command input", "profile set-default", "Profile name is required.", "Run agency profile set-default <profile>."))
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("set default profile", "InvalidProjectRoot", ".", err.Error(), "Run agency project init."))
			return 1
		}
		socket := defaultSupervisorSocket()
		result, err := supervisor.SetDefaultProfile(context.Background(), socket, supervisor.ProfileDefaultParams{CWD: cwd, Profile: args[1]})
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("set default profile", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("set default profile", "Storage error", args[1], err.Error(), "Run agency profile list."))
			return 1
		}
		fmt.Fprintln(stdout, "Default profile set")
		fmt.Fprintln(stdout, "project:", result.Project)
		fmt.Fprintln(stdout, "profile:", result.Profile)
		return 0
	default:
		return 2
	}
}

func runNew(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse new command", "Invalid command input", "new", "Provider is required.", "Run agency new codex or agency new claude."))
		return 2
	}
	providerKey := args[0]
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var title, cwd, profileKey, model, effort, permissionMode, sandboxMode, approvalPolicy, replayKey string
	var jsonOut, allowDangerous, commandPreview, worktree, noWorktree bool
	var worktreeName, baseRef string
	var addDirs []string
	var env []string
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&cwd, "cwd", "", "")
	fs.StringVar(&profileKey, "profile", "", "")
	fs.BoolVar(&worktree, "worktree", false, "")
	fs.BoolVar(&noWorktree, "no-worktree", false, "")
	fs.StringVar(&worktreeName, "worktree-name", "", "")
	fs.StringVar(&baseRef, "base", "", "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&effort, "effort", "", "")
	fs.StringVar(&permissionMode, "permission-mode", "", "")
	fs.StringVar(&sandboxMode, "sandbox", "", "")
	fs.StringVar(&approvalPolicy, "approval-policy", "", "")
	fs.StringVar(&replayKey, "replay-key", "", "")
	fs.Func("add-dir", "", func(value string) error {
		addDirs = append(addDirs, value)
		return nil
	})
	fs.Func("env", "", func(value string) error {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsAny(value, "\x00\n\r") {
			return fmt.Errorf("invalid environment assignment %q", value)
		}
		env = append(env, value)
		return nil
	})
	fs.BoolVar(&allowDangerous, "allow-dangerous", false, "")
	fs.BoolVar(&commandPreview, "command-preview", false, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("resolve cwd", "InvalidProjectRoot", ".", err.Error(), "Run agency new from a project directory."))
			return 1
		}
	}
	prompt := strings.Join(fs.Args(), " ")
	if title == "" {
		title = prompt
	}
	if title == "" {
		title = providerKey + " session"
	}
	if !commandPreview {
		socket := defaultSupervisorSocket()
		params := supervisor.StartSessionParams{
			Provider: providerKey, Profile: profileKey, Title: title, Prompt: prompt, CWD: cwd, Model: model, Effort: effort,
			PermissionMode: permissionMode, SandboxMode: sandboxMode, ApprovalPolicy: approvalPolicy,
			AddDirs: addDirs, Env: env, AllowDangerous: allowDangerous, Worktree: worktree, NoWorktree: noWorktree,
			WorktreeName: worktreeName, BaseRef: baseRef, ReplayKey: replayKey,
		}
		if params.ReplayKey == "" {
			copyParams := params
			copyParams.ReplayKey = ""
			params.ReplayKey = derivedReplayKey("startSession", copyParams)
		}
		result, err := supervisor.StartSession(context.Background(), socket, params)
		if err != nil {
			switch {
			case isDialError(err):
				fmt.Fprintln(stderr, content.ErrorText("start session", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			case errorMentions(err, "ProjectNotFound"):
				fmt.Fprintln(stderr, content.ErrorText("start session", "ProjectNotFound", cwd, err.Error(), "Run agency project init."))
				return 1
			case errorMentions(err, "DangerousLaunchBlocked", "SafetyDenied", "SafetyApprovalRequired"):
				fmt.Fprintln(stderr, content.ErrorText("launch "+strings.Title(providerKey), serverErrorReason(err), title, err.Error(), "Pass --allow-dangerous to request a dangerous mode."))
				return 3
			case errorMentions(err, "Unsupported", "ProviderNotFound", "ProviderCliMissing"):
				fmt.Fprintln(stderr, content.ErrorText("launch "+strings.Title(providerKey), serverErrorReason(err), title, err.Error(), "Choose a supported provider control."))
				return 1
			default:
				fmt.Fprintln(stderr, content.ErrorText("start session", "Launch failed", title, err.Error(), "Run agency doctor."))
				return 1
			}
		}
		if jsonOut {
			writeJSON(stdout, result)
			return 0
		}
		fmt.Fprintln(stdout, "Agent session started")
		fmt.Fprintln(stdout, "session:", result.Session)
		fmt.Fprintln(stdout, "provider:", result.Provider)
		fmt.Fprintln(stdout, "title:", result.Title)
		fmt.Fprintln(stdout, "workspace:", result.Workspace, "("+content.WorkspaceLabel(result.WorkspaceKey)+")")
		fmt.Fprintln(stdout, "path:", result.Path)
		fmt.Fprintln(stdout, "tmux:", result.Tmux)
		fmt.Fprintln(stdout, "model:", result.Model)
		fmt.Fprintln(stdout, "effort:", result.Effort)
		return 0
	}
	store, err := openDefaultStore()
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("open store", "Storage error", "agency.db", err.Error(), "Run agency doctor."))
		return 1
	}
	defer store.Close()
	project, err := store.ProjectForPath(context.Background(), cwd)
	if err != nil {
		gitRoot, rootErr := gitx.Root(context.Background(), cwd)
		if rootErr != nil {
			fmt.Fprintln(stderr, content.ErrorText("resolve project", "InvalidProjectRoot", cwd, rootErr.Error(), "Run agency project init from a git repository."))
			return 1
		}
		fmt.Fprintln(stderr, content.ErrorText("resolve project", "ProjectNotFound", gitRoot, err.Error(), "Run agency project init."))
		return 1
	}
	workspaceLabel := "project root"
	if (worktree || project.DefaultWorktreeMode == "Always") && !noWorktree {
		if baseRef == "" {
			baseRef = project.DefaultBaseRef
		}
		workspaceLabel = "managed worktree from " + baseRef
	}
	var gotProvider, defaultModel, defaultEffort, policy string
	if profileKey != "" {
		_, gotProvider, defaultModel, defaultEffort, policy, err = store.ProfileRevision(context.Background(), profileKey)
		if err == nil && gotProvider != providerKey {
			err = fmt.Errorf("profile %s is for provider %s, not %s", profileKey, gotProvider, providerKey)
		}
	} else {
		_, gotProvider, defaultModel, defaultEffort, policy, err = store.DefaultProfileRevision(context.Background(), project.ID, providerKey)
	}
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("resolve profile", "ProviderNotFound", providerKey, err.Error(), "Run agency profile list."))
		return 1
	}
	if model == "" {
		model = defaultModel
	}
	if effort == "" {
		effort = defaultEffort
	}
	if gotProvider == provider.KeyClaude && permissionMode == "" {
		permissionMode = "default"
	}
	if gotProvider == provider.KeyCodex {
		if sandboxMode == "" {
			sandboxMode = stringField(policy, "sandboxMode")
		}
		if approvalPolicy == "" {
			approvalPolicy = stringField(policy, "approvalPolicy")
		}
	}
	plan, err := provider.BuildLaunchPlan(provider.LaunchInput{
		ProviderKey: gotProvider, Model: model, Effort: effort, PermissionMode: permissionMode,
		SandboxMode: sandboxMode, ApprovalPolicy: approvalPolicy, AddDirs: addDirs, AllowDangerous: allowDangerous,
	})
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("launch "+strings.Title(gotProvider), reasonFromProviderError(err), "profile "+gotProvider+"_default", err.Error(), "Choose a supported provider control."))
		return 2
	}
	if commandPreview {
		fmt.Fprintln(stdout, "Command preview")
		fmt.Fprintln(stdout, "cwd:", cwd)
		fmt.Fprintln(stdout, "workspace:", workspaceLabel)
		fmt.Fprintln(stdout, "provider:", gotProvider)
		fmt.Fprintln(stdout, "model:", model)
		fmt.Fprintln(stdout, "effort:", effort)
		for _, pair := range env {
			name, _, _ := strings.Cut(pair, "=")
			fmt.Fprintln(stdout, "env:", name+"=<redacted>")
		}
		fmt.Fprintln(stdout, "argv:", plan.Preview)
		return 0
	}
	return 0
}

func runAdditional(args []string, stdout, stderr io.Writer) int {
	// run accepts the shared new/run flag set. An additional run resumes the
	// session's pinned workspace and profile revision, so control- and
	// workspace-changing flags cannot apply; they are accepted (not rejected as
	// unknown) and reported with a clear next action.
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var replayKey, title, cwd, worktreeName, baseRef, model, effort, permissionMode, sandboxMode, approvalPolicy string
	var jsonOut, worktree, noWorktree, allowDangerous, commandPreview bool
	fs.StringVar(&replayKey, "replay-key", "", "")
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&cwd, "cwd", "", "")
	fs.BoolVar(&worktree, "worktree", false, "")
	fs.BoolVar(&noWorktree, "no-worktree", false, "")
	fs.StringVar(&worktreeName, "worktree-name", "", "")
	fs.StringVar(&baseRef, "base", "", "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&effort, "effort", "", "")
	fs.StringVar(&permissionMode, "permission-mode", "", "")
	fs.StringVar(&sandboxMode, "sandbox", "", "")
	fs.StringVar(&approvalPolicy, "approval-policy", "", "")
	fs.BoolVar(&allowDangerous, "allow-dangerous", false, "")
	fs.BoolVar(&commandPreview, "command-preview", false, "")
	fs.Func("add-dir", "", func(string) error { return nil })
	fs.Func("env", "", func(string) error { return nil })
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse run command", "Invalid command input", "run", "Session handle is required.", "Run agency run <session>."))
		return 2
	}
	inapplicable := title != "" || cwd != "" || worktree || noWorktree || worktreeName != "" || baseRef != "" ||
		model != "" || effort != "" || permissionMode != "" || sandboxMode != "" || approvalPolicy != "" || allowDangerous || commandPreview
	if inapplicable {
		fmt.Fprintln(stderr, content.ErrorText("start run", "UnsupportedControl", args[len(args)-1],
			"Additional runs inherit the session's workspace and profile revision.",
			"Use agency new to launch with different controls."))
		return 2
	}
	session := fs.Arg(0)
	if replayKey == "" {
		replayKey = derivedReplayKey("startRun", supervisor.StartRunParams{Session: session})
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.StartRun(context.Background(), socket, session, replayKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("start run", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("start run", "SessionNotFound", session, err.Error(), "Run agency list."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, result)
		return 0
	}
	fmt.Fprintln(stdout, "Run requested")
	fmt.Fprintln(stdout, "session:", result.Session)
	fmt.Fprintln(stdout, "run:", result.Run)
	return 0
}

func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse list command", "Invalid command input", "list", "Too many arguments.", "Run agency list."))
		return 2
	}
	socket := defaultSupervisorSocket()
	sessions, err := supervisor.ListSessions(context.Background(), socket)
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText("list sessions", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
		return 4
	}
	if jsonOut {
		writeJSON(stdout, sessions)
		return 0
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No agent sessions in this project.")
		fmt.Fprintln(stdout, "Start one with agency new codex or agency new claude.")
		return 0
	}
	fmt.Fprintln(stdout, "SESSION     PROVIDER  TITLE              RUN            GIT     WORKSPACE     MODEL                 EFFORT  CLOSE")
	for _, s := range sessions {
		fmt.Fprintf(stdout, "%-10s  %-8s  %-17s  %-13s  %-6s  %-12s  %-20s  %-6s  %s\n",
			s.Session, s.Provider, trim(s.Title, 17), content.RunStatusLabel(s.RunStatus), content.GitSummaryLabel(gitSummary(s.Git)), content.WorkspaceLabel(s.WorkspaceKey), trim(s.Model, 20), s.Effort, content.CloseSummaryLabel(closeSummary(s.Close)))
	}
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse status command", "Invalid command input", "status", "Session handle is required.", "Run agency status <session>."))
		return 2
	}
	session := fs.Arg(0)
	socket := defaultSupervisorSocket()
	status, err := supervisor.SessionStatus(context.Background(), socket, session)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("read status", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		if strings.Contains(err.Error(), "SessionNotFound") {
			fmt.Fprintln(stderr, content.ErrorText("read status", "SessionNotFound", session, "No session matched that handle.", "Run agency list."))
			return 1
		}
		fmt.Fprintln(stderr, content.ErrorText("read status", "Supervisor API error", session, err.Error(), "Run agency doctor --supervisor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, status)
		return 0
	}
	fmt.Fprintln(stdout, "session:", status.Session)
	fmt.Fprintln(stdout, "provider:", status.Provider)
	fmt.Fprintln(stdout, "title:", status.Title)
	fmt.Fprintln(stdout, "run:", content.RunStatusLabel(status.RunStatus))
	fmt.Fprintln(stdout, "git:", content.GitSummaryLabel(gitSummary(status.Git)))
	fmt.Fprintln(stdout, "workspace:", status.Workspace, "("+content.WorkspaceLabel(status.WorkspaceKey)+")")
	fmt.Fprintln(stdout, "path:", status.Path)
	fmt.Fprintln(stdout, "model:", status.Model)
	fmt.Fprintln(stdout, "effort:", status.Effort)
	fmt.Fprintln(stdout, "close:", content.CloseSummaryLabel(closeSummary(status.Close)))
	if v := mapString(status.Launch, "permissionMode", ""); v != "" {
		fmt.Fprintln(stdout, "permission-mode:", v)
	}
	if v := mapString(status.Launch, "sandboxMode", ""); v != "" {
		fmt.Fprintln(stdout, "sandbox:", v)
	}
	if v := mapString(status.Launch, "approvalPolicy", ""); v != "" {
		fmt.Fprintln(stdout, "approval-policy:", v)
	}
	if target := mapString(status.Tmux, "target", ""); target != "" {
		fmt.Fprintln(stdout, "tmux:", target)
	}
	if available, ok := status.Diff["available"].(bool); ok && available {
		fmt.Fprintln(stdout, "diff: available")
	} else {
		fmt.Fprintln(stdout, "diff: unavailable")
	}
	if len(status.Events) > 0 {
		fmt.Fprintln(stdout, "events:", len(status.Events))
	}
	if len(status.RecentOutput) > 0 {
		text := outputText(status.RecentOutput)
		fmt.Fprintln(stdout, "recent output:")
		fmt.Fprint(stdout, text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

func outputText(chunks []storage.OutputChunk) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.Write(chunk.Bytes)
	}
	return b.String()
}

func runEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse events command", "Invalid command input", "events", "Session handle is required.", "Run agency events <session>."))
		return 2
	}
	session := fs.Arg(0)
	socket := defaultSupervisorSocket()
	events, err := supervisor.SessionEvents(context.Background(), socket, session)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("read events", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		if strings.Contains(err.Error(), "SessionNotFound") {
			fmt.Fprintln(stderr, content.ErrorText("read events", "SessionNotFound", session, "No session matched that handle.", "Run agency list."))
			return 1
		}
		fmt.Fprintln(stderr, content.ErrorText("read events", "Supervisor API error", session, err.Error(), "Run agency doctor --supervisor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, events)
		return 0
	}
	if len(events) == 0 {
		fmt.Fprintln(stdout, "No events recorded for this session.")
		return 0
	}
	fmt.Fprintln(stdout, "TIME                      SUBJECT  EVENT")
	for _, event := range events {
		fmt.Fprintf(stdout, "%-24s  %-7s  %s\n", trim(event.OccurredAt, 24), trim(mapString(event.Subject, "kind", "Subject"), 7), eventlog.Label(event.EventType))
	}
	return 0
}

func runDiff(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse diff command", "Invalid command input", "diff", "Session handle is required.", "Run agency diff <session>."))
		return 2
	}
	socket := defaultSupervisorSocket()
	diff, err := supervisor.SessionDiff(context.Background(), socket, args[0])
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("show diff", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("show diff", "Git error", args[0], err.Error(), "Run agency status "+args[0]+"."))
		return 1
	}
	fmt.Fprint(stdout, diff.Stat)
	if diff.Stat != "" && !strings.HasSuffix(diff.Stat, "\n") {
		fmt.Fprintln(stdout)
	}
	fmt.Fprint(stdout, diff.Patch)
	return 0
}

func runAttach(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse attach command", "Invalid command input", "attach", "Session handle is required.", "Run agency attach <session>."))
		return 2
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.AttachCommand(context.Background(), socket, args[0])
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("attach session", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		if errorMentions(err, "LostTmuxTarget", "LostRunner", "RepairRequired") {
			fmt.Fprintln(stderr, content.ErrorText("attach session", "Lost tmux target", args[0], err.Error(), "Run agency doctor."))
			return 5
		}
		if strings.Contains(err.Error(), "SessionClosed") {
			fmt.Fprintln(stderr, content.ErrorText("attach session", "SessionClosed", args[0], "Closed sessions cannot be attached.", "Run agency list."))
			return 1
		}
		fmt.Fprintln(stderr, content.ErrorText("attach session", "SessionNotFound", args[0], err.Error(), "Run agency list."))
		return 1
	}
	if !realStdio(stdout, stderr) {
		fmt.Fprintln(stdout, strings.Join(result.Argv, " "))
		return 0
	}
	cmd := exec.Command(result.Argv[0], result.Argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(stderr, content.ErrorText("attach session", "TmuxAttachFailed", args[0], err.Error(), "Run agency status "+args[0]+"."))
		return 1
	}
	return 0
}

func runSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var replayKey string
	fs.StringVar(&replayKey, "replay-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	args = fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse send command", "Invalid command input", "send", "Session handle is required.", "Run agency send <session> <message>."))
		return 2
	}
	var input []byte
	if len(args) == 1 {
		var err error
		input, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("send input", "InputReadFailed", args[0], err.Error(), "Pass a message argument or pipe stdin."))
			return 1
		}
		if len(input) == 0 {
			fmt.Fprintln(stderr, content.ErrorText("parse send command", "Invalid command input", "send", "Message or stdin is required.", "Run agency send <session> <message>."))
			return 2
		}
	} else {
		input = []byte(strings.Join(args[1:], " ") + "\n")
	}
	if replayKey == "" {
		// Each interactive send is a distinct logical action. Deriving the replay
		// key from the input bytes would make two identical keystrokes (pressing
		// Enter twice, sending "y" twice) collide on one key, so the second would
		// be treated as a replay and silently dropped. A fresh per-invocation
		// nonce keeps them distinct; --replay-key overrides for scripted retries.
		replayKey = "sendInput:" + storage.NewID()
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.SendInput(context.Background(), socket, args[0], input, replayKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("send input", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		if strings.Contains(err.Error(), "ReplayKey") || strings.Contains(err.Error(), "ReplayInFlight") {
			fmt.Fprintln(stderr, content.ErrorText("send input", "Replay error", replayKey, err.Error(), "Use the same replay key only for the same request."))
			return 1
		}
		fmt.Fprintln(stderr, content.ErrorText("send input", "RunNotLive", args[0], err.Error(), "Run agency status "+args[0]+"."))
		return 1
	}
	fmt.Fprintln(stdout, "Input accepted")
	fmt.Fprintln(stdout, "session:", args[0])
	fmt.Fprintln(stdout, "run:", result.Run)
	fmt.Fprintln(stdout, "input:", result.InputSeq)
	return 0
}

func runRename(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var replayKey string
	fs.StringVar(&replayKey, "replay-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	args = fs.Args()
	if len(args) < 2 {
		fmt.Fprintln(stderr, content.ErrorText("parse rename command", "Invalid command input", "rename", "Session handle and title are required.", "Run agency rename <session> <title>."))
		return 2
	}
	title := strings.Join(args[1:], " ")
	if replayKey == "" {
		replayKey = derivedReplayKey("renameSession", supervisor.RenameSessionParams{Session: args[0], Title: title})
	}
	socket := defaultSupervisorSocket()
	if err := supervisor.RenameSession(context.Background(), socket, args[0], title, replayKey); err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("rename session", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("rename session", "SessionNotFound", args[0], err.Error(), "Run agency list."))
		return 1
	}
	fmt.Fprintln(stdout, "Session renamed")
	fmt.Fprintln(stdout, "session:", args[0])
	fmt.Fprintln(stdout, "title:", title)
	return 0
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func runStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var replayKey string
	fs.StringVar(&replayKey, "replay-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse stop command", "Invalid command input", "stop", "Session handle is required.", "Run agency stop <session>."))
		return 2
	}
	session := fs.Arg(0)
	if replayKey == "" {
		replayKey = derivedReplayKey("stopRun", supervisor.StopRunParams{Session: session})
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.StopRun(context.Background(), socket, session, replayKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("stop run", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("stop run", "RunNotLive", session, err.Error(), "Run agency status "+session+"."))
		return 1
	}
	fmt.Fprintln(stdout, "Stop requested")
	fmt.Fprintln(stdout, "session:", session)
	fmt.Fprintln(stdout, "run:", result.Run)
	fmt.Fprintln(stdout, "signal:", "graceful")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Run stopped")
	fmt.Fprintln(stdout, "session:", session)
	fmt.Fprintln(stdout, "run:", result.Run)
	return 0
}

func runKill(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse kill command", "Invalid command input", "kill", "Session handle is required.", "Run agency kill <session>."))
		return 2
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.KillRun(context.Background(), socket, args[0])
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("kill run", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("kill run", "RunNotLive", args[0], err.Error(), "Run agency status "+args[0]+"."))
		return 1
	}
	fmt.Fprintln(stdout, "Run killed")
	fmt.Fprintln(stdout, "session:", args[0])
	fmt.Fprintln(stdout, "run:", result.Run)
	return 0
}

func runClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var terminal bool
	var replayKey string
	fs.BoolVar(&terminal, "terminal", false, "")
	fs.StringVar(&replayKey, "replay-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	socket := defaultSupervisorSocket()
	if terminal {
		if replayKey == "" {
			replayKey = derivedReplayKey("closeTerminalSessions", supervisor.CloseTerminalSessionsParams{})
		}
		n, err := supervisor.CloseTerminalSessions(context.Background(), socket, replayKey)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("close terminal sessions", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("close terminal sessions", "Storage error", "sessions", err.Error(), "Run agency doctor."))
			return 1
		}
		fmt.Fprintln(stdout, "Terminal sessions closed")
		fmt.Fprintln(stdout, "count:", n)
		return 0
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse close command", "Invalid command input", "close", "Session handle is required.", "Run agency close <session>."))
		return 2
	}
	session := fs.Arg(0)
	if replayKey == "" {
		replayKey = derivedReplayKey("closeSession", supervisor.CloseSessionParams{Session: session})
	}
	result, err := supervisor.CloseSession(context.Background(), socket, session, replayKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("close session", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		if err.Error() == "SessionStillRunning" {
			fmt.Fprintln(stderr, "Close session "+session+"?")
			fmt.Fprintln(stderr)
			fmt.Fprintln(stderr, "The run is still live.")
			fmt.Fprintln(stderr)
			fmt.Fprintln(stderr, "Choose one:")
			fmt.Fprintln(stderr, "1. Detach only")
			fmt.Fprintln(stderr, "2. Stop run, then close session")
			fmt.Fprintln(stderr, "3. Cancel")
			return 3
		}
		fmt.Fprintln(stderr, content.ErrorText("close session", "SessionNotFound", session, err.Error(), "Run agency list."))
		return 1
	}
	fmt.Fprintln(stdout, "Session closed")
	fmt.Fprintln(stdout, "session:", result.Session)
	fmt.Fprintln(stdout, "workspace:", result.Workspace, "("+content.WorkspaceLabel(result.WorkspaceKey)+") remains active")
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut, supervisorOnly bool
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.BoolVar(&supervisorOnly, "supervisor", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse doctor command", "Invalid command input", "doctor", "Too many arguments.", "Run agency doctor."))
		return 2
	}
	if supervisorOnly {
		health, err := supervisor.HealthCheck(context.Background(), defaultSupervisorSocket())
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("read supervisor health", "RemoteSupervisorUnavailable", defaultSupervisorSocket(), err.Error(), "Start agency-supervisor."))
			return 4
		}
		writeJSON(stdout, health)
		return 0
	}
	socket := defaultSupervisorSocket()
	report, err := supervisor.Doctor(context.Background(), socket)
	if err != nil {
		if isDialError(err) {
			if jsonOut {
				fmt.Fprintln(stderr, content.ErrorText("read doctor observations", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stdout, "Doctor observations")
			if _, err := os.Stat(defaultStatePath()); err == nil {
				fmt.Fprintln(stdout, "state: present")
			} else {
				fmt.Fprintln(stdout, "state: missing")
			}
			for _, name := range []string{"git", "tmux", "claude", "codex"} {
				if path, err := execLookPath(name); err == nil {
					fmt.Fprintf(stdout, "%s: %s\n", name, path)
				} else {
					fmt.Fprintf(stdout, "%s: missing\n", name)
				}
			}
			fmt.Fprintln(stdout, "supervisor: unavailable")
			return 0
		}
		fmt.Fprintln(stderr, content.ErrorText("read doctor observations", "Supervisor API error", socket, err.Error(), "Run agency doctor --supervisor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, report)
		return 0
	}
	fmt.Fprintln(stdout, "Doctor observations")
	fmt.Fprintln(stdout, "state:", report.StateDB)
	if len(report.Issues) == 0 {
		fmt.Fprintln(stdout, "issues: none")
		return 0
	}
	for _, issue := range report.Issues {
		fmt.Fprintln(stdout, "issue:", issue.Code)
		if session, ok := issue.Target["session"].(string); ok {
			fmt.Fprintln(stdout, "session:", session)
		}
		if run, ok := issue.Target["run"].(string); ok {
			fmt.Fprintln(stdout, "run:", run)
		}
		if env, ok := issue.Target["env"].(string); ok {
			fmt.Fprintln(stdout, "env:", env)
		}
		if provider, ok := issue.Target["provider"].(string); ok {
			fmt.Fprintln(stdout, "provider:", provider)
		}
		if command, ok := issue.Target["command"].(string); ok {
			fmt.Fprintln(stdout, "command:", command)
		}
		if workspace, ok := issue.Target["workspace"].(string); ok {
			fmt.Fprintln(stdout, "workspace:", workspace)
		}
		if project, ok := issue.Target["project"].(string); ok {
			fmt.Fprintln(stdout, "project:", project)
		}
		if branch, ok := issue.Target["branch"].(string); ok {
			fmt.Fprintln(stdout, "branch:", branch)
		}
		if path, ok := issue.Target["path"].(string); ok {
			fmt.Fprintln(stdout, "path:", path)
		}
		fmt.Fprintln(stdout, "summary:", issue.Summary)
		if issue.Repair != "" {
			fmt.Fprintln(stdout, "repair:", issue.Repair)
		}
	}
	return 0
}

func runHost(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "install-unit" {
		return runHostInstallUnit(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "check" {
		return runHostSSH(args[1:], stdout, stderr, "check", remote.BootstrapArgv)
	}
	if len(args) >= 1 && args[0] == "discover" {
		return runHostSSH(args[1:], stdout, stderr, "discover", remote.DiscoverArgv)
	}
	if len(args) >= 1 && args[0] == "start" {
		return runHostSSH(args[1:], stdout, stderr, "start", remote.StartArgv)
	}
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(stderr, content.ErrorText("parse host command", "Invalid command input", "host", "Expected list, check, discover, start, or install-unit.", "Run agency host list."))
		return 2
	}
	fs := flag.NewFlagSet("host list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse host command", "Invalid command input", "host list", "Too many arguments.", "Run agency host list."))
		return 2
	}
	socket := defaultSupervisorSocket()
	hosts, err := supervisor.ListHosts(context.Background(), socket)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("list hosts", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("list hosts", "Storage error", "hosts", err.Error(), "Run agency doctor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, hosts)
		return 0
	}
	fmt.Fprintln(stdout, "HOST        MODE            ALIAS")
	for _, host := range hosts {
		alias := host.Access.HostAlias
		if alias == "" {
			alias = "-"
		}
		fmt.Fprintf(stdout, "%-11s %-15s %s\n", host.Key, host.Access.Mode, alias)
	}
	return 0
}

func runHostInstallUnit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("host install-unit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var launchd bool
	fs.BoolVar(&launchd, "launchd", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	binary := "agency-supervisor"
	if resolved, err := execLookPath("agency-supervisor"); err == nil {
		binary = resolved
	}
	if launchd {
		fmt.Fprintln(stdout, "# Write to ~/Library/LaunchAgents/com.agency.supervisor.plist, then:")
		fmt.Fprintln(stdout, "# launchctl load ~/Library/LaunchAgents/com.agency.supervisor.plist")
		fmt.Fprint(stdout, remote.LaunchdAgent(binary))
		return 0
	}
	fmt.Fprintln(stdout, "# Write to ~/.config/systemd/user/agency-supervisor.service, then:")
	fmt.Fprintln(stdout, "# systemctl --user enable --now agency-supervisor && loginctl enable-linger")
	fmt.Fprint(stdout, remote.SystemdUserUnit(binary))
	return 0
}

// runHostSSH resolves a configured host to its SSH alias and runs a validated
// remote command (bootstrap check, supervisor discovery, or start). Mosh hosts
// are attach-only; local hosts need no SSH.
func runHostSSH(args []string, stdout, stderr io.Writer, action string, buildArgv func(string) ([]string, error)) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse host command", "Invalid command input", "host "+action, "Host key is required.", "Run agency host "+action+" <host>."))
		return 2
	}
	hostKey := args[0]
	socket := defaultSupervisorSocket()
	hosts, err := supervisor.ListHosts(context.Background(), socket)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText(action+" host", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText(action+" host", "Storage error", "hosts", err.Error(), "Run agency doctor."))
		return 1
	}
	var target *storage.Host
	for i := range hosts {
		if hosts[i].Key == hostKey {
			target = &hosts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintln(stderr, content.ErrorText(action+" host", "HostNotFound", hostKey, "No such host is configured.", "Run agency host list."))
		return 1
	}
	if target.Access.Mode == "Local" {
		fmt.Fprintln(stdout, "Host", hostKey, "is local; no SSH is required.")
		return 0
	}
	if target.Access.Mode == "AttachOnlyMosh" {
		fmt.Fprintln(stdout, "Host "+hostKey+" is attach-only.")
		fmt.Fprintln(stdout, "Control-plane operations require SSH access to the remote supervisor.")
		return 0
	}
	argv, err := buildArgv(target.Access.HostAlias)
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText(action+" host", "InvalidHostAlias", hostKey, err.Error(), "Fix the host access alias in config."))
		return 1
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintln(stderr, content.ErrorText(action+" host", "SshCommandFailed", hostKey, strings.TrimSpace(string(out))+" "+err.Error(), "Verify SSH access to "+target.Access.HostAlias+"."))
		return 1
	}
	fmt.Fprintf(stdout, "host %s (%s) %s ok\n", hostKey, target.Access.HostAlias, action)
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		fmt.Fprintln(stdout, trimmed)
	}
	return 0
}

func runWorktree(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse worktree command", "Invalid command input", "worktree", "Missing worktree subcommand.", "Run agency worktree list."))
		return 2
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		var cwd string
		fs.BoolVar(&jsonOut, "json", false, "")
		fs.StringVar(&cwd, "project", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, content.ErrorText("parse worktree list command", "Invalid command input", "worktree list", "Too many arguments.", "Run agency worktree list."))
			return 2
		}
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fmt.Fprintln(stderr, content.ErrorText("list worktrees", "InvalidProjectRoot", ".", err.Error(), "Run agency project init."))
				return 1
			}
		}
		socket := defaultSupervisorSocket()
		worktrees, err := supervisor.ListWorktrees(context.Background(), socket, cwd)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("list worktrees", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("list worktrees", "ProjectNotFound", cwd, err.Error(), "Run agency project init."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, worktrees)
			return 0
		}
		if len(worktrees) == 0 {
			fmt.Fprintln(stdout, "No managed worktrees in this project.")
			fmt.Fprintln(stdout, "Start a worktree session with agency new codex --worktree.")
			return 0
		}
		fmt.Fprintln(stdout, "WORKTREE      HANDLE      BRANCH              GIT     CLOSE            SESSIONS  MARKER  DIFF")
		for _, wt := range worktrees {
			marker := "bad"
			if matches, ok := wt.Marker["matches"].(bool); ok && matches {
				marker = "ok"
			}
			diff := "unavailable"
			if available, ok := wt.Diff["available"].(bool); ok && available {
				diff = "available"
			}
			fmt.Fprintf(stdout, "%-13s %-11s %-19s %-7s %-16s %-9d %-7s %s\n",
				content.WorkspaceLabel(wt.WorkspaceKey),
				wt.Workspace,
				trim(wt.Branch, 19),
				content.GitSummaryLabel(gitSummary(wt.Git)),
				content.CloseSummaryLabel(closeSummary(wt.Close)),
				len(wt.Sessions),
				marker,
				diff,
			)
		}
		return 0
	case "status":
		fs := flag.NewFlagSet("worktree status", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		fs.BoolVar(&jsonOut, "json", false, "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, content.ErrorText("parse worktree status command", "Invalid command input", "worktree status", "Workspace key is required.", "Run agency worktree status <workspace>."))
			return 2
		}
		socket := defaultSupervisorSocket()
		result, err := supervisor.WorktreeStatus(context.Background(), socket, fs.Arg(0))
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("read worktree status", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("read worktree status", "Safety check failed", fs.Arg(0), err.Error(), "Run agency doctor."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, result)
			return 0
		}
		fmt.Fprintln(stdout, "worktree:", result.Workspace, "("+content.WorkspaceLabel(result.WorkspaceKey)+")")
		fmt.Fprintln(stdout, "path:", result.Path)
		fmt.Fprintln(stdout, "branch:", result.Branch)
		fmt.Fprintln(stdout, "base:", result.BaseRef, result.BaseSHA)
		fmt.Fprintln(stdout, "git:", content.GitSummaryLabel(gitSummary(result.Git)))
		fmt.Fprintln(stdout, "close:", content.CloseSummaryLabel(mapString(result.Close, "summary", "RepairRequired")))
		if len(result.Sessions) > 0 {
			fmt.Fprintln(stdout, "sessions:", strings.Join(result.Sessions, ", "))
		}
		if matches, ok := result.Marker["matches"].(bool); ok {
			fmt.Fprintln(stdout, "marker:", matches)
		}
		if available, ok := result.Diff["available"].(bool); ok && available {
			fmt.Fprintln(stdout, "diff: available")
		} else {
			fmt.Fprintln(stdout, "diff: unavailable")
		}
		return 0
	case "diff":
		if len(args) != 2 {
			fmt.Fprintln(stderr, content.ErrorText("parse worktree diff command", "Invalid command input", "worktree diff", "Workspace key is required.", "Run agency worktree diff <workspace>."))
			return 2
		}
		socket := defaultSupervisorSocket()
		diff, err := supervisor.WorktreeDiff(context.Background(), socket, args[1])
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("show worktree diff", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("show worktree diff", "Git error", args[1], err.Error(), "Run agency worktree status "+args[1]+"."))
			return 1
		}
		fmt.Fprint(stdout, diff.Stat)
		if diff.Stat != "" && !strings.HasSuffix(diff.Stat, "\n") {
			fmt.Fprintln(stdout)
		}
		fmt.Fprint(stdout, diff.Patch)
		return 0
	case "close":
		fs := flag.NewFlagSet("worktree close", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var jsonOut bool
		var replayKey string
		fs.BoolVar(&jsonOut, "json", false, "")
		fs.StringVar(&replayKey, "replay-key", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, content.ErrorText("parse worktree close command", "Invalid command input", "worktree close", "Workspace key is required.", "Run agency worktree close <workspace>."))
			return 2
		}
		socket := defaultSupervisorSocket()
		workspace := fs.Arg(0)
		if replayKey == "" {
			replayKey = derivedReplayKey("closeWorktree", supervisor.CloseWorktreeParams{Workspace: workspace})
		}
		result, err := supervisor.CloseWorktree(context.Background(), socket, workspace, replayKey)
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("close worktree", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			if strings.Contains(err.Error(), "ManagedWorktreeNotFound") {
				fmt.Fprintln(stderr, content.ErrorText("close worktree", "ManagedWorktreeNotFound", workspace, err.Error(), "Run agency worktree list."))
				return 1
			}
			fmt.Fprintln(stderr, content.ErrorText("close worktree", "Safety check failed", workspace, err.Error(), "Run agency doctor."))
			return 1
		}
		if jsonOut {
			writeJSON(stdout, result)
			if result.Status == "Blocked" {
				return 3
			}
			return 0
		}
		if result.Status == "Blocked" {
			fmt.Fprintln(stderr, "Worktree close blocked.")
			fmt.Fprintln(stderr)
			fmt.Fprintln(stderr, "blockers:")
			for _, blocker := range result.Blockers {
				closeBlocker := gitx.CloseBlocker(blocker.Blocker)
				fmt.Fprintln(stderr, "-", content.CloseSummaryLabel(string(gitx.SummarizeClose([]gitx.CloseBlocker{closeBlocker})))+": "+blocker.Summary)
			}
			fmt.Fprintln(stderr)
			fmt.Fprintln(stderr, "No files were removed.")
			return 3
		}
		fmt.Fprintln(stdout, "Managed worktree removed")
		fmt.Fprintln(stdout, "workspace:", result.Workspace, "("+content.WorkspaceLabel(result.WorkspaceKey)+")")
		fmt.Fprintln(stdout, "path:", result.Path)
		fmt.Fprintln(stdout, "branch:", result.Branch, "remains")
		return 0
	default:
		fmt.Fprintln(stderr, content.ErrorText("parse worktree command", "Invalid command input", args[0], "Unknown worktree subcommand.", "Run agency worktree list."))
		return 2
	}
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, content.ErrorText("parse config command", "Invalid command input", "config", "Expected get or set with a key.", "Run agency config get paths.state_dir."))
		return 2
	}
	switch args[0] {
	case "get":
		if len(args) != 2 {
			fmt.Fprintln(stderr, content.ErrorText("read config", "Invalid command input", "config get", "Expected one config key.", "Run agency config get defaults.profile."))
			return 2
		}
		switch args[1] {
		case "paths.state_db":
			fmt.Fprintln(stdout, defaultStatePath())
			return 0
		}
		if !supportedConfigKey(args[1]) {
			fmt.Fprintln(stderr, content.ErrorText("read config", "Invalid command input", args[1], "Unknown config key.", "Run agency config get defaults.profile."))
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("read config", "InvalidProjectRoot", ".", err.Error(), "Run agency project init."))
			return 1
		}
		socket := defaultSupervisorSocket()
		result, err := supervisor.GetConfig(context.Background(), socket, supervisor.ConfigGetParams{CWD: cwd, Key: args[1]})
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("read config", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("read config", "Storage error", args[1], err.Error(), "Run agency project init."))
			return 1
		}
		fmt.Fprintln(stdout, result.Value)
		return 0
	case "set":
		fs := flag.NewFlagSet("config set", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var replayKey string
		fs.StringVar(&replayKey, "replay-key", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		rest := fs.Args()
		if len(rest) != 2 {
			fmt.Fprintln(stderr, content.ErrorText("write config", "Invalid command input", "config set", "Expected key and value.", "Run agency config set defaults.profile <profile>."))
			return 2
		}
		if !supportedConfigKey(rest[0]) {
			fmt.Fprintln(stderr, content.ErrorText("write config", "Invalid command input", rest[0], "Unknown config key.", "Run agency config set defaults.profile <profile>."))
			return 2
		}
		if key, ok := hostAccessConfigKey(rest[0]); ok {
			if _, err := storage.ParseHostAccess(rest[1]); err != nil || !storage.ValidHostKey(key) {
				detail := "Host access must be local, ssh:<alias>, or mosh:<alias>."
				if err != nil {
					detail = err.Error()
				}
				fmt.Fprintln(stderr, content.ErrorText("write config", "Invalid command input", rest[1], detail, "Run agency config set hosts.devbox.access ssh:devbox."))
				return 2
			}
		}
		if rest[0] == "defaults.host" && !storage.ValidHostKey(rest[1]) {
			fmt.Fprintln(stderr, content.ErrorText("write config", "Invalid command input", rest[1], "Host key is not valid.", "Run agency host list."))
			return 2
		}
		if rest[0] == "defaults.worktree_mode" {
			if _, err := storage.ParseWorktreeMode(rest[1]); err != nil {
				fmt.Fprintln(stderr, content.ErrorText("write config", "Invalid command input", rest[1], "Worktree mode must be prompt, always, or never.", "Run agency config set defaults.worktree_mode prompt."))
				return 2
			}
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, content.ErrorText("write config", "InvalidProjectRoot", ".", err.Error(), "Run agency project init."))
			return 1
		}
		if replayKey == "" {
			replayKey = derivedReplayKey("setConfig", supervisor.ConfigSetParams{CWD: cwd, Key: rest[0], Value: rest[1]})
		}
		socket := defaultSupervisorSocket()
		result, err := supervisor.SetConfig(context.Background(), socket, supervisor.ConfigSetParams{CWD: cwd, Key: rest[0], Value: rest[1], ReplayKey: replayKey})
		if err != nil {
			if isDialError(err) {
				fmt.Fprintln(stderr, content.ErrorText("write config", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
				return 4
			}
			fmt.Fprintln(stderr, content.ErrorText("write config", "Storage error", rest[0], err.Error(), "Run agency profile list."))
			return 1
		}
		fmt.Fprintln(stdout, "Config value set")
		fmt.Fprintln(stdout, "key:", result.Key)
		fmt.Fprintln(stdout, "value:", result.Value)
		return 0
	default:
		fmt.Fprintln(stderr, content.ErrorText("parse config command", "Invalid command input", args[0], "Unknown config subcommand.", "Run agency config get paths.state_db."))
		return 2
	}
}

func hostAccessConfigKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "hosts.") || !strings.HasSuffix(key, ".access") {
		return "", false
	}
	hostKey := strings.TrimSuffix(strings.TrimPrefix(key, "hosts."), ".access")
	if !storage.ValidHostKey(hostKey) {
		return "", false
	}
	return hostKey, true
}

func supportedConfigKey(key string) bool {
	if key == "defaults.profile" || key == "defaults.base_ref" || key == "defaults.host" || key == "defaults.worktree_mode" {
		return true
	}
	_, ok := hostAccessConfigKey(key)
	return ok
}

func derivedReplayKey(operation string, value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return operation + ":" + hex.EncodeToString(sum[:])
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, content.ErrorText("parse prune command", "Invalid command input", "prune", "Too many arguments.", "Run agency prune."))
		return 2
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.Prune(context.Background(), socket)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("prune state", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("prune state", "Storage error", "agency.db", err.Error(), "Run agency doctor."))
		return 1
	}
	fmt.Fprintln(stdout, "State pruned")
	fmt.Fprintln(stdout, "idempotency keys:", result.IdempotencyKeys)
	fmt.Fprintln(stdout, "notification deliveries:", result.NotificationDeliveries)
	fmt.Fprintln(stdout, "close attempts:", result.CloseAttempts)
	fmt.Fprintln(stdout, "safety checks:", result.SafetyCheckRuns)
	fmt.Fprintln(stdout, "output chunks:", result.OutputChunks)
	fmt.Fprintln(stdout, "status snapshots:", result.StatusSnapshots)
	fmt.Fprintln(stdout, "events:", result.Events)
	return 0
}

func runExport(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse export command", "Invalid command input", "export", "Output path is required.", "Run agency export <path>."))
		return 2
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.Export(context.Background(), socket, args[0])
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("export state", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("export state", "Storage error", args[0], err.Error(), "Choose another output path."))
		return 1
	}
	fmt.Fprintln(stdout, "State exported")
	fmt.Fprintln(stdout, "path:", result.Path)
	return 0
}

func runRepair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var replayKey string
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	fs.StringVar(&replayKey, "replay-key", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, content.ErrorText("parse repair command", "Invalid command input", "repair", "Repair key is required.", "Run agency doctor."))
		return 2
	}
	key := fs.Arg(0)
	if replayKey == "" {
		replayKey = derivedReplayKey("repair", supervisor.RepairParams{Key: key})
	}
	socket := defaultSupervisorSocket()
	result, err := supervisor.Repair(context.Background(), socket, key, replayKey)
	if err != nil {
		if isDialError(err) {
			fmt.Fprintln(stderr, content.ErrorText("repair state", "RemoteSupervisorUnavailable", socket, err.Error(), "Start agency-supervisor."))
			return 4
		}
		fmt.Fprintln(stderr, content.ErrorText("repair state", "Repair failed", key, err.Error(), "Run agency doctor."))
		return 1
	}
	if jsonOut {
		writeJSON(stdout, result)
		return 0
	}
	fmt.Fprintln(stdout, "Repair completed")
	fmt.Fprintln(stdout, "repair:", result.Key)
	fmt.Fprintln(stdout, "session:", result.Session)
	fmt.Fprintln(stdout, "status:", result.Status)
	return 0
}

func openDefaultStore() (*storage.Store, error) {
	return storage.Open(context.Background(), defaultStatePath())
}

func defaultStatePath() string {
	if v := os.Getenv("AGENCY_STATE_DB"); v != "" {
		return v
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "agency", "agency.db")
}

func defaultSupervisorSocket() string {
	if v := os.Getenv("AGENCY_SUPERVISOR_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(privateRuntimeDir(), "agency", "supervisor.sock")
}

// privateRuntimeDir returns a per-user runtime directory for the API socket. It
// never falls back to the world-shared system temp directory, where another
// user could pre-create agency/ and MITM the socket; it prefers
// XDG_RUNTIME_DIR, then the per-uid /run/user/<uid>, then the private user cache
// dir, then a private ~/.agency/run. The supervisor additionally asserts the
// socket's parent directory is 0700 before binding (assertPrivateSocketDir).
func privateRuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		return base
	}
	perUID := fmt.Sprintf("/run/user/%d", os.Getuid())
	if info, err := os.Stat(perUID); err == nil && info.IsDir() {
		return perUID
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		return cache
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".agency", "run")
	}
	// A uid-scoped subdirectory under temp as the last resort. It is not shared
	// with other users' agency instances, and the supervisor still refuses to
	// bind if the resulting socket directory is wider than 0700.
	return filepath.Join(os.TempDir(), fmt.Sprintf("agency-%d", os.Getuid()))
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func stringField(raw, key string) string {
	var data map[string]string
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return ""
	}
	return data[key]
}

func mapString(row map[string]any, key, fallback string) string {
	value, ok := row[key].(string)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func gitSummary(row map[string]any) string {
	value, ok := row["summary"].(string)
	if !ok || value == "" {
		return "NotAWorktree"
	}
	return value
}

func closeSummary(row map[string]any) string {
	value, ok := row["summary"].(string)
	if !ok || value == "" {
		return "RepairRequired"
	}
	return value
}

func reasonFromProviderError(err error) string {
	var inputErr provider.InputError
	if ok := asProviderError(err, &inputErr); ok {
		return reasonForCode(inputErr.Code)
	}
	return serverErrorReason(err)
}

// reasonForCode maps a typed operation code to a human reason for content.ErrorText.
func reasonForCode(code string) string {
	switch code {
	case "UnsupportedModel":
		return "Unsupported model"
	case "UnsupportedEffort":
		return "Unsupported effort"
	case "UnsupportedControl":
		return "Unsupported control"
	case "DangerousLaunchBlocked", "SafetyDenied":
		return "Safety denied"
	case "SafetyApprovalRequired":
		return "Safety approval required"
	case "ProviderCliMissing":
		return "Provider CLI missing"
	case "ProviderNotFound":
		return "Provider not found"
	default:
		return code
	}
}

// serverErrorReason recovers the typed code an operation error carries across
// the supervisor boundary (errors serialize as "Code: detail") and maps it to a
// human reason. Only meaningful once the error is known to carry a typed code.
func serverErrorReason(err error) string {
	if err == nil {
		return ""
	}
	code, _, ok := strings.Cut(err.Error(), ":")
	if !ok {
		code = err.Error()
	}
	return reasonForCode(strings.TrimSpace(code))
}

// errorMentions reports whether the error text contains any of the typed codes.
// Operation errors cross the supervisor boundary as strings, so the CLI must
// classify them textually to honor the exit-code contract.
func errorMentions(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range codes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

func asProviderError(err error, target *provider.InputError) bool {
	if err == nil {
		return false
	}
	if v, ok := err.(provider.InputError); ok {
		*target = v
		return true
	}
	return false
}

func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func execLookPath(file string) (string, error) {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		full := filepath.Join(dir, file)
		info, err := os.Stat(full)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return full, nil
		}
	}
	return "", os.ErrNotExist
}

func realStdio(stdout, stderr io.Writer) bool {
	return stdout == os.Stdout && stderr == os.Stderr
}
