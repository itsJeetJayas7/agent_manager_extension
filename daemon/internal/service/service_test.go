package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeSystemctl(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("AC_SERVICE_TEST_LOG", log)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AC_SERVICE_TEST_LOG\"\n"
	if fail {
		script += "echo 'user bus unavailable' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SHELL", "")
	return log
}

func TestSystemdInstallPropagatesFailure(t *testing.T) {
	fakeSystemctl(t, true)
	i := &Installer{Home: t.TempDir()}
	if _, err := i.installSystemd("/tmp/conductor"); err == nil || !strings.Contains(err.Error(), "user bus unavailable") {
		t.Fatalf("false installation success: %v", err)
	}
}

func TestSystemdUpgradeRestartsAndQuotesPaths(t *testing.T) {
	log := fakeSystemctl(t, false)
	i := &Installer{Home: t.TempDir()}
	r, err := i.installSystemd("/tmp/a space/100%/conductor")
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "--user daemon-reload\n--user enable agent-conductor.service\n--user restart agent-conductor.service\n" {
		t.Fatalf("upgrade did not restart: %s", calls)
	}
	unit, _ := os.ReadFile(r.UnitPath)
	if !strings.Contains(string(unit), `ExecStart="/tmp/a space/100%%/conductor" serve`) || !strings.Contains(string(unit), "KillMode=control-group") {
		t.Fatalf("unsafe unit: %s", unit)
	}
}

func TestLaunchdEscapesPaths(t *testing.T) {
	if got := xmlText("/Users/A&B/<local>"); got != "/Users/A&amp;B/&lt;local&gt;" {
		t.Fatal(got)
	}
}
