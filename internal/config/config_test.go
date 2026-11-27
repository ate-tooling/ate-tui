package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, body string
		want       []string
		wantErr    bool
	}{
		{"missing", "", nil, false},
		{"contexts", "writeContexts: [kind-dev, gke_proj_us-central1_staging]\n", []string{"kind-dev", "gke_proj_us-central1_staging"}, false},
		{"typo", "writeContext: [kind-dev]\n", nil, true},
	}
	for _, tt := range tests {
		path := filepath.Join(dir, tt.name+".yaml")
		if tt.body != "" {
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c, err := Load(path)
		if (err != nil) != tt.wantErr || !slices.Equal(c.WriteContexts, tt.want) {
			t.Errorf("%s: got %v, %v", tt.name, c.WriteContexts, err)
		}
	}
}

func TestXDGPaths(t *testing.T) {
	d := For("ate-tui")
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := d.JournalPath(); got != "/state/ate-tui/journal.jsonl" {
		t.Errorf("JournalPath = %s", got)
	}
	if got := d.Path(); got != "/cfg/ate-tui/config.yaml" {
		t.Errorf("Path = %s", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := d.JournalPath(); got != "/home/u/.local/state/ate-tui/journal.jsonl" {
		t.Errorf("JournalPath = %s", got)
	}
}

func TestLoadFullConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `refresh: 5s
defaultView: workers
atespace: research
skin: catppuccin-latte
colors:
  red: "#ff0000"
keys:
  suspend: S
  sort: ctrl+s
mouse: false
proxyAddress: 0.0.0.0
writeContexts: [kind-dev]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Refresh.String() != "5s" || c.DefaultView != "workers" || c.Atespace != "research" || c.Skin != "catppuccin-latte" ||
		c.Colors.Red != "#ff0000" || c.Keys["suspend"] != "S" || c.Mouse || c.ProxyAddress != "0.0.0.0" {
		t.Errorf("config %+v", c)
	}
	for _, body := range []string{"", "atespace: research\n"} {
		if body != "" {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(path); err != nil || !c.Mouse {
			t.Errorf("%q: mouse is off by default (%v)", body, err)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"refresh-typo":  "refresh: 5 seconds\n",
		"refresh-small": "refresh: 10ms\n",
		"proxy-host":    "proxyAddress: example.com\n",
		"color-typo":    "colors: {rouge: \"#ff0000\"}\n",
		"bare-bool-key": "keys: {yaml: Y}\n",
	} {
		path := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
