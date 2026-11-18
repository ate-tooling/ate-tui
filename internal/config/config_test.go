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
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := JournalPath(); got != "/state/ate-tui/journal.jsonl" {
		t.Errorf("JournalPath = %s", got)
	}
	if got := Path(); got != "/cfg/ate-tui/config.yaml" {
		t.Errorf("Path = %s", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := JournalPath(); got != "/home/u/.local/state/ate-tui/journal.jsonl" {
		t.Errorf("JournalPath = %s", got)
	}
}
