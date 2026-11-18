package skin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsAreComplete(t *testing.T) {
	for name, p := range Builtin {
		if err := p.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mine", "base: catppuccin-latte\ncolors:\n  red: \"#ff0000\"\n")
	write("bad", "colors:\n  red: crimson\n")
	write("typo", "colours: {}\n")

	p, err := Resolve("", dir, Palette{})
	if err != nil || p != Builtin[Default] {
		t.Errorf("default: %v %v", p, err)
	}
	p, err = Resolve("mine", dir, Palette{Blue: "#0000ff"})
	if err != nil {
		t.Fatal(err)
	}
	latte := Builtin["catppuccin-latte"]
	if p.Red != "#ff0000" || p.Blue != "#0000ff" || p.Text != latte.Text {
		t.Errorf("mine: %+v", p)
	}
	for name, want := range map[string]string{
		"bad":     "want #rrggbb",
		"typo":    "unknown field",
		"missing": "unknown skin",
	} {
		if _, err := Resolve(name, dir, Palette{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", name, err, want)
		}
	}
}
