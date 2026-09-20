// Package service installs the daemon as a persistent user service:
// systemd --user on Linux (survives SSH logout with lingering) and
// LaunchAgent on macOS. No root; no secrets in unit definitions.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// InstallResult reports what was installed.
type InstallResult struct {
	Mode       string   `json:"mode"` // systemd | launchd
	UnitPath   string   `json:"unitPath"`
	BinPath    string   `json:"binPath"`
	LingerNote string   `json:"lingerNote,omitempty"`
	Commands   []string `json:"commands"`
}

// Installer plans and applies the service installation.
type Installer struct {
	Home      string
	BinSource string // current binary path
	Version   string
}

// TargetDir is ~/.local/share/agent-conductor/bin/<version>.
func (i *Installer) TargetDir() string {
	return filepath.Join(i.Home, ".local", "share", "agent-conductor", "bin", i.Version)
}

// StablePath is the launcher location on PATH.
func (i *Installer) StablePath() string {
	return filepath.Join(i.Home, ".local", "bin", "agent-conductor")
}

// Install copies the binary and writes the service unit.
func (i *Installer) Install() (*InstallResult, error) {
	if err := os.MkdirAll(i.TargetDir(), 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(i.TargetDir(), "agent-conductor")
	if abs, err := filepath.Abs(i.BinSource); err == nil {
		i.BinSource = abs
	}
	if i.BinSource != target {
		data, err := os.ReadFile(i.BinSource)
		if err != nil {
			return nil, fmt.Errorf("read binary: %w", err)
		}
		// Write to a temp path and rename: overwriting a RUNNING binary fails
		// with ETXTBSY on Linux, and rename is atomic for readers.
		tmp := target + ".new"
		if err := os.WriteFile(tmp, data, 0o755); err != nil {
			return nil, err
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			os.Remove(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("install binary: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(i.StablePath()), 0o755); err != nil {
		return nil, err
	}
	// Remove then link: os.Symlink fails with EEXIST when the link is already
	// there, which silently left upgrades pointing at the OLD binary.
	if err := os.Remove(i.StablePath()); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("replace launcher %s: %w", i.StablePath(), err)
	}
	if err := os.Symlink(target, i.StablePath()); err != nil {
		return nil, fmt.Errorf("link launcher %s: %w", i.StablePath(), err)
	}

	switch runtime.GOOS {
	case "linux":
		return i.installSystemd(target)
	case "darwin":
		return i.installLaunchd(target)
	default:
		return nil, fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
}

func (i *Installer) installSystemd(bin string) (*InstallResult, error) {
	unitDir := filepath.Join(i.Home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return nil, err
	}
	unit := filepath.Join(unitDir, "agent-conductor.service")
	tpl := template.Must(template.New("unit").Parse(`[Unit]
Description=Agent Conductor background daemon
After=network.target

[Service]
ExecStart={{.Bin}} serve
Environment=PATH={{.Path}}
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`))
	f, err := os.Create(unit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := tpl.Execute(f, map[string]string{"Bin": bin, "Path": i.servicePATH()}); err != nil {
		return nil, err
	}
	res := &InstallResult{Mode: "systemd", UnitPath: unit, BinPath: bin,
		Commands: []string{"systemctl --user daemon-reload", "systemctl --user enable --now agent-conductor.service"}}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "--now", "agent-conductor.service").Run()
	// Detect lingering honestly; never require or use sudo.
	if lingerEnabled() {
		res.LingerNote = "lingering enabled: service survives SSH logout"
	} else {
		res.LingerNote = "lingering NOT enabled. For overnight VPS execution after SSH logout run: loginctl enable-linger $USER (may require admin; Agent Conductor will not run it for you)"
	}
	return res, nil
}

// lingerEnabled reports whether the user's systemd session lingers.
//
// This used to pass the literal string "$USER" to loginctl, which never
// expands outside a shell, so the check always failed and fell through to a
// shell fallback.
func lingerEnabled() bool {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return false
	}
	out, err := exec.Command("loginctl", "show-user", u.Username, "--property=Linger").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "linger=yes")
}

// servicePATH is an explicit PATH for the service.
//
// LaunchAgents and systemd user units inherit a minimal PATH
// (/usr/bin:/bin:/usr/sbin:/sbin), so findBinary("claude") missed nvm,
// npm-global, asdf and volta installs - which is most of them. The login
// shell's PATH is captured at install time and merged in.
func (i *Installer) servicePATH() string {
	seen := map[string]bool{}
	var parts []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		parts = append(parts, p)
	}
	for _, p := range strings.Split(loginShellPATH(), ":") {
		add(p)
	}
	for _, p := range []string{
		filepath.Join(i.Home, ".local", "bin"),
		filepath.Join(i.Home, ".npm-global", "bin"),
		filepath.Join(i.Home, ".volta", "bin"),
		filepath.Join(i.Home, ".asdf", "shims"),
		filepath.Join(i.Home, ".bun", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	} {
		add(p)
	}
	return strings.Join(parts, ":")
}

// loginShellPATH asks the user's login shell for its PATH, so version
// managers that only configure interactive shells are still found.
func loginShellPATH() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return os.Getenv("PATH")
	}
	out, err := exec.Command(shell, "-l", "-c", "printf %s \"$PATH\"").Output()
	if err != nil || len(out) == 0 {
		return os.Getenv("PATH")
	}
	return strings.TrimSpace(string(out))
}

func (i *Installer) installLaunchd(bin string) (*InstallResult, error) {
	dir := filepath.Join(i.Home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	plist := filepath.Join(dir, "local.agent-conductor.daemon.plist")
	tpl := template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>local.agent-conductor.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Bin}}</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>{{.Path}}</string>
    <key>HOME</key>
    <string>{{.Home}}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>{{.LogDir}}/service.out.log</string>
  <key>StandardErrorPath</key>
  <string>{{.LogDir}}/service.err.log</string>
</dict>
</plist>
`))
	logDir := filepath.Join(i.Home, ".agent-conductor", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Create(plist)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := tpl.Execute(f, map[string]string{
		"Bin": bin, "Path": i.servicePATH(), "Home": i.Home, "LogDir": logDir,
	}); err != nil {
		return nil, err
	}
	exec.Command("launchctl", "unload", plist).Run()
	if err := exec.Command("launchctl", "load", plist).Run(); err != nil {
		return nil, fmt.Errorf("launchctl load: %w", err)
	}
	return &InstallResult{Mode: "launchd", UnitPath: plist, BinPath: bin,
		Commands: []string{"launchctl list | grep agent-conductor"}}, nil
}

// Uninstall stops and removes the service (not the state directory).
func (i *Installer) Uninstall() (notes []string, err error) {
	switch runtime.GOOS {
	case "linux":
		exec.Command("systemctl", "--user", "disable", "--now", "agent-conductor.service").Run()
		unit := filepath.Join(i.Home, ".config", "systemd", "user", "agent-conductor.service")
		os.Remove(unit)
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return []string{"systemd user service removed"}, nil
	case "darwin":
		plist := filepath.Join(i.Home, "Library", "LaunchAgents", "local.agent-conductor.daemon.plist")
		exec.Command("launchctl", "unload", plist).Run()
		os.Remove(plist)
		return []string{"launch agent removed"}, nil
	default:
		return nil, fmt.Errorf("unsupported platform")
	}
}

// Status returns human-readable service status.
func Status() string {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("systemctl", "--user", "is-active", "agent-conductor.service").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
		return "inactive"
	case "darwin":
		if out, err := exec.Command("sh", "-c", "launchctl list | grep agent-conductor || true").Output(); err == nil && len(out) > 0 {
			return strings.TrimSpace(string(out))
		}
		return "inactive"
	default:
		return "unsupported"
	}
}
