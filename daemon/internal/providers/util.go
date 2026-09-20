package providers

import (
	"os"
	"os/exec"
	"path/filepath"
)

// findBinary locates an executable on PATH and common install locations.
func findBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".local", "share", name, "bin", name),
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
		"/usr/bin/" + name,
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}
