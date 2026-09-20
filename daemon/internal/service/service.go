// Package service installs the daemon as a persistent user service:
// systemd --user on Linux (survives SSH logout with lingering) and
// LaunchAgent on macOS. No root; no secrets in unit definitions.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// InstallResult reports what was installed.
type InstallResult struct {
	Mode         string `json:"mode"` // systemd | launchd
	UnitPath     string `json:"unitPath"`
	BinPath      string `json:"binPath"`
	LingerNote   string `json:"lingerNote,omitempty"`
	Commands     []string `json:"commands"`
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
		if err := os.WriteFile(target, data, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(i.StablePath()), 0o755); err != nil {
		return nil, err
	}
	_ = os.Symlink(target, i.StablePath())

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
	if err := tpl.Execute(f, map[string]string{"Bin": bin}); err != nil {
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

func lingerEnabled() bool {
	out, err := exec.Command("loginctl", "show-user", "$USER").Output()
	if err != nil {
		// $USER won't expand; try actual user
		out, err = exec.Command("sh", "-c", "loginctl show-user $USER | grep -i linger").Output()
		if err != nil {
			return false
		}
	}
	return strings.Contains(strings.ToLower(string(out)), "linger=yes")
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
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`))
	f, err := os.Create(plist)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := tpl.Execute(f, map[string]string{"Bin": bin}); err != nil {
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
