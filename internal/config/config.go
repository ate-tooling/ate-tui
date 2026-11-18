// Package config reads ~/.config/ate-tui/config.yaml. Flags override it.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ate-tooling/ate-tui/internal/skin"
	"sigs.k8s.io/yaml"
)

type Config struct {
	// Refresh is the poll interval, as a Go duration ("2s").
	Refresh string `json:"refresh,omitempty"`
	// DefaultView is the kind shown at start ("workers").
	DefaultView string `json:"defaultView,omitempty"`
	// Atespace scopes the start view; empty is every atespace.
	Atespace string `json:"atespace,omitempty"`
	// Skin is a built-in skin or skins/<name>.yaml next to this file.
	Skin string `json:"skin,omitempty"`
	// Colors override single skin colors.
	Colors skin.Palette `json:"colors,omitempty"`
	// Keys rebinds table keys: action name → key.
	Keys map[string]string `json:"keys,omitempty"`
	// Mouse turns mouse support on (the default) or off.
	Mouse *bool `json:"mouse,omitempty"`
	// ProxyAddress is where f forwards listen (default 127.0.0.1).
	ProxyAddress string `json:"proxyAddress,omitempty"`
	// WriteContexts are kube contexts where ate-tui allows writes.
	WriteContexts []string `json:"writeContexts,omitempty"`
}

// MinRefresh keeps a typo from polling ateapi in a tight loop.
const MinRefresh = 250 * time.Millisecond

// RefreshInterval is Refresh parsed, or 0 when unset.
func (c Config) RefreshInterval() (time.Duration, error) {
	if c.Refresh == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.Refresh)
	if err != nil {
		return 0, fmt.Errorf("refresh: %w", err)
	}
	if d < MinRefresh {
		return 0, fmt.Errorf("refresh: %s is below the %s minimum", d, MinRefresh)
	}
	return d, nil
}

// MouseEnabled reports whether the mouse is on.
func (c Config) MouseEnabled() bool { return c.Mouse == nil || *c.Mouse }

// Path is $XDG_CONFIG_HOME/ate-tui/config.yaml, or ~/.config/ate-tui/config.yaml.
func Path() string { return xdg("XDG_CONFIG_HOME", ".config", "config.yaml") }

// SkinDir holds custom skins, next to the config file.
func SkinDir() string { return xdg("XDG_CONFIG_HOME", ".config", "skins") }

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

// Load reads path. A missing file is an empty config. Unknown fields and
// values that cannot work are errors, so a typo does not go unnoticed.
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
	for action, key := range c.Keys {
		// YAML 1.1 reads unquoted y, Y, n, N, yes, no, on and off as
		// booleans, which arrive here as "true" or "false".
		if key == "true" || key == "false" {
			return c, fmt.Errorf(`%s: keys: %s: YAML reads an unquoted y, Y, n, N, on or off as %s; quote the key, e.g. %s: "Y"`, path, action, key, action)
		}
	}
	if _, err := c.RefreshInterval(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if a := c.ProxyAddress; a != "" && a != "localhost" && net.ParseIP(a) == nil {
		return c, fmt.Errorf("%s: proxyAddress %q is not an IP address", path, a)
	}
	return c, nil
}
