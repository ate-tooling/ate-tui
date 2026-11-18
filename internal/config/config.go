// Package config reads ~/.config/ate-tui/config.yaml. For now it holds only
// the kube contexts where writes are allowed without --write.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

type Config struct {
	// WriteContexts are kube contexts where ate-tui allows writes.
	WriteContexts []string `json:"writeContexts"`
}

// Path is $XDG_CONFIG_HOME/ate-tui/config.yaml, or ~/.config/ate-tui/config.yaml.
func Path() string { return xdg("XDG_CONFIG_HOME", ".config", "config.yaml") }

// JournalPath is $XDG_STATE_HOME/ate-tui/journal.jsonl, or
// ~/.local/state/ate-tui/journal.jsonl.
func JournalPath() string {
	return xdg("XDG_STATE_HOME", filepath.Join(".local", "state"), "journal.jsonl")
}

func xdg(env, fallback, file string) string {
	dir := os.Getenv(env)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, fallback)
	}
	return filepath.Join(dir, "ate-tui", file)
}

// Load reads path. A missing file is an empty config.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
