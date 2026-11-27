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
	// Refresh is the poll interval, at least MinRefresh; 0 when unset. The
	// file spells it as a Go duration ("2s").
	Refresh time.Duration `json:"-"`
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
	// Mouse is on unless the file turns it off.
	Mouse bool `json:"-"`
	// ProxyAddress is where f forwards listen (default 127.0.0.1).
	ProxyAddress string `json:"proxyAddress,omitempty"`
	// WriteContexts are kube contexts where ate-tui allows writes.
	WriteContexts []string `json:"writeContexts,omitempty"`
}

// file is config.yaml as written: the fields whose spelling differs from
// Config's take their place.
type file struct {
	Config
	Refresh string `json:"refresh,omitempty"`
	Mouse   *bool  `json:"mouse,omitempty"`
}

// MinRefresh keeps a typo from polling ateapi in a tight loop.
const MinRefresh = 250 * time.Millisecond

// Dirs are where a program keeps its files, in the XDG base directories,
// each in a directory named after the program.
type Dirs struct{ program string }

// For is the directories of the program named program.
func For(program string) Dirs { return Dirs{program} }

// Path is $XDG_CONFIG_HOME/<program>/config.yaml, or
// ~/.config/<program>/config.yaml.
func (d Dirs) Path() string { return d.xdg("XDG_CONFIG_HOME", ".config", "config.yaml") }

// SkinDir holds custom skins, next to the config file.
func (d Dirs) SkinDir() string { return d.xdg("XDG_CONFIG_HOME", ".config", "skins") }

// JournalPath is $XDG_STATE_HOME/<program>/journal.jsonl, or
// ~/.local/state/<program>/journal.jsonl.
func (d Dirs) JournalPath() string {
	return d.xdg("XDG_STATE_HOME", filepath.Join(".local", "state"), "journal.jsonl")
}

func (d Dirs) xdg(env, fallback, file string) string {
	dir := os.Getenv(env)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, fallback)
	}
	return filepath.Join(dir, d.program, file)
}

// Load reads path. A missing file is an empty config. Unknown fields and
// values that cannot work are errors, so a typo does not go unnoticed.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{Mouse: true}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var f file
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c, err := f.check()
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// check turns f into a Config, or says which value cannot work.
func (f file) check() (Config, error) {
	c := f.Config
	for action, key := range c.Keys {
		// YAML 1.1 reads unquoted y, Y, n, N, yes, no, on and off as
		// booleans, which arrive here as "true" or "false".
		if key == "true" || key == "false" {
			return c, fmt.Errorf(`keys: %s: YAML reads an unquoted y, Y, n, N, on or off as %s; quote the key, e.g. %s: "Y"`, action, key, action)
		}
	}
	if f.Refresh != "" {
		d, err := time.ParseDuration(f.Refresh)
		if err != nil {
			return c, fmt.Errorf("refresh: %w", err)
		}
		if d < MinRefresh {
			return c, fmt.Errorf("refresh: %s is below the %s minimum", d, MinRefresh)
		}
		c.Refresh = d
	}
	if a := c.ProxyAddress; a != "" && a != "localhost" && net.ParseIP(a) == nil {
		return c, fmt.Errorf("proxyAddress %q is not an IP address", a)
	}
	c.Mouse = f.Mouse == nil || *f.Mouse
	return c, nil
}
