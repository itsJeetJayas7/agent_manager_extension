// Command agent-conductor is the persistent Agent Conductor daemon and its
// operational subcommands (service install, hook bridge, diagnostics).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"agent-conductor/internal/api"
	"agent-conductor/internal/claudewrap"
	"agent-conductor/internal/db"
	"agent-conductor/internal/hooks"
	"agent-conductor/internal/hooksbridge"
	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
	"agent-conductor/internal/quota"
	"agent-conductor/internal/scheduler"
	"agent-conductor/internal/service"
	"agent-conductor/internal/vault"
	"agent-conductor/internal/worktrees"
)

// Version is stamped at build time from package.json:
//
//	go build -ldflags "-X main.Version=$(node -p 'require("./package.json").version')"
//
// It used to be a const duplicated in three places.
var Version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(os.Getenv("AC_MOCK") == "1")
	case "hook":
		if len(args) < 2 {
			die("usage: agent-conductor hook <claude|codex> [--gate]")
		}
		os.Exit(hooksbridge.Run(args[1], args[2:]...))
	case "claude-wrapper":
		os.Exit(runClaudeWrapper(args[1:]))
	case "install-claude-wrapper":
		var path string
		if path, err = claudewrap.Install(root()); err == nil {
			fmt.Println(path)
		}
	case "completion-gate":
		// Explicit alias that always enables the managed completion gate.
		if len(args) < 2 {
			die("usage: agent-conductor completion-gate <claude|codex>")
		}
		os.Exit(hooksbridge.Run(args[1], append(args[2:], "--gate")...))
	case "install-service":
		err = installService()
	case "uninstall-service":
		err = uninstallService()
	case "status":
		fmt.Printf("daemon socket: %s\n", api.DefaultPaths(root()).Socket)
		fmt.Printf("service: %s\n", service.Status())
	case "diagnostics":
		err = runDiagnostics()
	case "repair-hooks":
		err = repairHooks()
	case "version":
		fmt.Printf("agent-conductor %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	default:
		die("unknown command " + cmd)
	}
	if err != nil {
		die(err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "agent-conductor:", msg)
	os.Exit(1)
}

func root() string {
	if r := os.Getenv("AC_ROOT"); r != "" {
		return r
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent-conductor")
}

func serve(mock bool) error {
	paths := api.DefaultPaths(root())
	for _, d := range []string{paths.Root, filepath.Dir(paths.DB), paths.Logs, filepath.Dir(paths.Socket)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	// Exactly one daemon per root. A second one used to steal the socket while
	// the first kept scheduling against the same database.
	lock, err := api.AcquireInstanceLock(paths.Root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-conductor:", err)
		fmt.Fprintln(os.Stderr, "the running daemon is already serving this root; nothing to do.")
		return nil
	}
	defer lock.Release()

	redactor := logging.NewRedactor()
	log, err := logging.New(filepath.Join(paths.Logs, "daemon.jsonl"), logging.Info, redactor)
	if err != nil {
		return err
	}
	defer log.Close()
	log.Infof("agent-conductor daemon %s starting (mock=%v)", Version, mock)

	store, err := db.Open(paths.DB)
	if err != nil {
		return err
	}
	defer store.Close()

	vv, err := vault.Open(paths.Key)
	if err != nil {
		return err
	}
	// register any existing secret block with the redactor
	if v, _ := store.LatestSecretVersion(); v != nil {
		if plain, err := vv.Open(v.Cipher()); err == nil {
			redactor.Add(plain)
		}
	}

	hookBus := providers.NewHookBus()
	selfBin, _ := os.Executable()

	registry := providers.NewRegistry()
	registry.Register(providers.NewClaude(log, hookBus, selfBin, paths.Socket))
	registry.Register(providers.NewCodex(log, ""))
	if mock {
		registry.Register(providers.NewFake())
		log.Infof("mock provider registered (AC_MOCK=1)")
	}

	guardian := quota.New()
	wt := worktrees.NewManager(paths.Worktrees)
	conductor := scheduler.New(store, vv, registry, guardian, wt, hookBus, log, scheduler.ShellVerifier{})

	// restore persisted quota observations
	restoreQuota(store, guardian)

	claudewrap.Refresh(paths.Root)

	srv := api.NewServer(paths, store, conductor, registry, guardian, hookBus, log, Version)
	if err := srv.Listen(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conductor.Start(ctx)
	go func() {
		if err := srv.Serve(); err != nil {
			log.Errorf("serve: %v", err)
			cancel()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Infof("shutting down")
	conductor.Stop()
	for _, a := range registry.All() {
		_ = a.Close()
	}
	srv.Close()
	return nil
}

func restoreQuota(store *db.Store, g *quota.Guardian) {
	now := time.Now().UTC()
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		rows, err := store.ListQuotaWindows(p)
		if err != nil {
			continue
		}
		var ws []providers.QuotaWindow
		for _, r := range rows {
			w := providers.QuotaWindow{ID: r.WindowID, Name: r.Name, UsedPercent: r.UsedPercent,
				HasPercent: true, Exhausted: r.Exhausted, Source: r.Source, Confidence: r.Confidence}
			if r.ResetsAt != "" {
				if t, err := time.Parse(time.RFC3339, r.ResetsAt); err == nil {
					w.ResetsAt, w.HasReset = t, true
				}
			}
			ws = append(ws, w)
		}
		if len(ws) > 0 {
			g.Observe(p, ws, now)
		}
	}
}

func installService() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	inst := &service.Installer{Home: home, BinSource: self, Version: Version}
	res, err := inst.Install()
	if err != nil {
		return err
	}
	fmt.Printf("installed %s service\n  unit: %s\n  binary: %s\n", res.Mode, res.UnitPath, res.BinPath)
	for _, c := range res.Commands {
		fmt.Printf("  verify: %s\n", c)
	}
	if res.LingerNote != "" {
		fmt.Printf("\n%s\n", res.LingerNote)
	}
	// install provider awareness hooks (non-destructive)
	if err := repairHooksAt(inst.StablePath()); err != nil {
		fmt.Printf("warning: provider hooks not installed: %v\n", err)
	}
	return nil
}

func uninstallService() error {
	home, _ := os.UserHomeDir()
	inst := &service.Installer{Home: home, BinSource: "", Version: Version}
	notes, err := inst.Uninstall()
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	// remove ONLY our namespaced provider hook entries
	hm := hooks.New(mustSelf(), home)
	if r, err := hm.UninstallClaude(); err == nil && r.Changed {
		fmt.Printf("claude hooks removed (%s)\n", r.Path)
	}
	if r, err := hm.UninstallCodex(); err == nil && r.Changed {
		fmt.Printf("codex hooks removed (%s)\n", r.Path)
	}
	fmt.Println("state kept at " + root() + " (delete manually to erase all runs/secrets)")
	return nil
}

func repairHooks() error {
	return repairHooksAt(mustSelf())
}

func repairHooksAt(binary string) error {
	home, _ := os.UserHomeDir()
	hm := hooks.New(binary, home)
	r1, err := hm.InstallClaude()
	if err != nil {
		fmt.Printf("claude hooks: %v\n", err)
	} else {
		fmt.Printf("claude hooks: %s (%s)\n", r1.Path, noteOr(r1))
	}
	r2, err := hm.InstallCodex()
	if err != nil {
		fmt.Printf("codex hooks: %v\n", err)
	} else {
		fmt.Printf("codex hooks: %s (%s)\n", r2.Path, noteOr(r2))
	}
	if err1, err2 := r1.Note, r2.Note; strings.Contains(err1, "not valid") || strings.Contains(err2, "not valid") {
		return fmt.Errorf("existing provider config unreadable; manual repair needed")
	}
	return nil
}

func noteOr(r hooks.InstallResult) string {
	if r.Changed {
		if r.Backup != "" {
			return "installed, backup " + r.Backup
		}
		return "installed"
	}
	return r.Note
}

func mustSelf() string {
	self, err := os.Executable()
	if err != nil {
		return "agent-conductor"
	}
	return self
}

// runtimeLine reports one provider runtime: its version, and where it was
// found when that is somewhere a user would not think to look.
func runtimeLine(name, installURL string) string {
	bin := providers.FindRuntime(name)
	if bin == "" {
		return name + ": NOT FOUND (install: " + installURL + ")"
	}
	v, err := exec.Command(bin, "--version").CombinedOutput()
	version := strings.TrimSpace(string(v))
	if err != nil || version == "" {
		version = "installed"
	}
	line := name + ": " + version
	if onPath, err := exec.LookPath(name); err != nil || onPath != bin {
		line += " (" + bin + ")"
	}
	return line
}

func runDiagnostics() error {
	paths := api.DefaultPaths(root())
	out := []string{}
	out = append(out, fmt.Sprintf("agent-conductor %s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH))
	out = append(out, fmt.Sprintf("root: %s", paths.Root))
	out = append(out, fmt.Sprintf("service: %s", service.Status()))
	// Discovery goes through the provider package so diagnostics and the
	// sidebar never disagree about whether a runtime is installed; a CLI that
	// only exists inside an editor extension bundle counts as installed.
	out = append(out, runtimeLine("claude", "https://claude.com/claude-code"))
	out = append(out, runtimeLine("codex", "https://developers.openai.com/codex/"))
	out = append(out, fmt.Sprintf("socket: %s (exists=%v)", paths.Socket, fileExists(paths.Socket)))
	out = append(out, fmt.Sprintf("database: %s (exists=%v)", paths.DB, fileExists(paths.DB)))
	out = append(out, fmt.Sprintf("vault key: %s (exists=%v)", paths.Key, fileExists(paths.Key)))
	fmt.Println(strings.Join(out, "\n"))
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
