// Package remote owns devbox/remote-host support that is pure and host-agnostic:
// auto-start unit generation (systemd user unit, launchd agent) and construction
// of validated SSH command argv. SSH command lines are built as token arrays and
// never assembled by string concatenation; the host alias is validated so it
// cannot inject shell.
package remote

import (
	"fmt"
	"regexp"
)

// validAlias matches an SSH host alias: letters, digits, dot, underscore, dash.
// It deliberately excludes whitespace and shell metacharacters.
var validAlias = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateAlias reports an error if the alias is empty or contains characters
// that could inject shell or SSH options.
func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("ssh host alias is required")
	}
	if !validAlias.MatchString(alias) {
		return fmt.Errorf("ssh host alias %q contains invalid characters", alias)
	}
	return nil
}

// BootstrapArgv builds the argv to verify SSH access to a devbox. It runs a
// trivial remote command; success proves reachable, authenticated SSH.
func BootstrapArgv(alias string) ([]string, error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	return []string{"ssh", "-o", "BatchMode=yes", alias, "true"}, nil
}

// DiscoverArgv builds the argv to discover a remote supervisor's health over SSH.
func DiscoverArgv(alias string) ([]string, error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	return []string{"ssh", "-o", "BatchMode=yes", alias, "agency", "doctor", "--supervisor", "--json"}, nil
}

// StartArgv builds the argv to start the remote supervisor via its systemd user
// unit (installed by `agency host install-unit`).
func StartArgv(alias string) ([]string, error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	return []string{"ssh", "-o", "BatchMode=yes", alias, "systemctl", "--user", "start", "agency-supervisor"}, nil
}

// ForwardSocketArgv builds the argv to forward the remote supervisor's Unix
// socket to a local path over SSH, delegating authentication to SSH.
func ForwardSocketArgv(alias, localPath, remotePath string) ([]string, error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	if localPath == "" || remotePath == "" {
		return nil, fmt.Errorf("local and remote socket paths are required")
	}
	return []string{"ssh", "-N", "-L", localPath + ":" + remotePath, alias}, nil
}

// SystemdUserUnit returns the content of a systemd user unit that auto-starts the
// supervisor on login/boot (with lingering enabled).
func SystemdUserUnit(supervisorBinary string) string {
	if supervisorBinary == "" {
		supervisorBinary = "agency-supervisor"
	}
	return `[Unit]
Description=agency supervisor
After=default.target

[Service]
Type=simple
ExecStart=` + supervisorBinary + `
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`
}

// LaunchdAgent returns the content of a launchd user agent plist that auto-starts
// the supervisor at login and keeps it alive.
func LaunchdAgent(supervisorBinary string) string {
	if supervisorBinary == "" {
		supervisorBinary = "agency-supervisor"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agency.supervisor</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + supervisorBinary + `</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`
}
