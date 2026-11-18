// Package skin holds the color palettes ate-tui can draw with. A palette
// names the Catppuccin colors the UI uses; a custom skin file overrides any
// of them on top of a built-in.
package skin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"sigs.k8s.io/yaml"
)

// Palette is the set of colors the UI draws with, as "#rrggbb".
type Palette struct {
	Red      string `json:"red,omitempty"`
	Peach    string `json:"peach,omitempty"`
	Yellow   string `json:"yellow,omitempty"`
	Green    string `json:"green,omitempty"`
	Teal     string `json:"teal,omitempty"`
	Sky      string `json:"sky,omitempty"`
	Blue     string `json:"blue,omitempty"`
	Lavender string `json:"lavender,omitempty"`
	Mauve    string `json:"mauve,omitempty"`
	Pink     string `json:"pink,omitempty"`
	Text     string `json:"text,omitempty"`
	Subtext0 string `json:"subtext0,omitempty"`
	Overlay2 string `json:"overlay2,omitempty"`
	Overlay1 string `json:"overlay1,omitempty"`
	Overlay0 string `json:"overlay0,omitempty"`
	Surface1 string `json:"surface1,omitempty"`
	Base     string `json:"base,omitempty"`
}

// Default is the skin sofka uses.
const Default = "catppuccin-mocha"

// Builtin are the four Catppuccin flavors.
var Builtin = map[string]Palette{
	"catppuccin-mocha": {
		Red: "#f38ba8", Peach: "#fab387", Yellow: "#f9e2af", Green: "#a6e3a1", Teal: "#94e2d5",
		Sky: "#89dceb", Blue: "#89b4fa", Lavender: "#b4befe", Mauve: "#cba6f7", Pink: "#f5c2e7",
		Text: "#cdd6f4", Subtext0: "#a6adc8", Overlay2: "#9399b2", Overlay1: "#7f849c",
		Overlay0: "#6c7086", Surface1: "#45475a", Base: "#1e1e2e",
	},
	"catppuccin-macchiato": {
		Red: "#ed8796", Peach: "#f5a97f", Yellow: "#eed49f", Green: "#a6da95", Teal: "#8bd5ca",
		Sky: "#91d7e3", Blue: "#8aadf4", Lavender: "#b7bdf8", Mauve: "#c6a0f6", Pink: "#f5bde6",
		Text: "#cad3f5", Subtext0: "#a5adcb", Overlay2: "#939ab7", Overlay1: "#8087a2",
		Overlay0: "#6e738d", Surface1: "#494d64", Base: "#24273a",
	},
	"catppuccin-frappe": {
		Red: "#e78284", Peach: "#ef9f76", Yellow: "#e5c890", Green: "#a6d189", Teal: "#81c8be",
		Sky: "#99d1db", Blue: "#8caaee", Lavender: "#babbf1", Mauve: "#ca9ee6", Pink: "#f4b8e4",
		Text: "#c6d0f5", Subtext0: "#a5adce", Overlay2: "#949cbb", Overlay1: "#838ba7",
		Overlay0: "#737994", Surface1: "#51576d", Base: "#303446",
	},
	"catppuccin-latte": {
		Red: "#d20f39", Peach: "#fe640b", Yellow: "#df8e1d", Green: "#40a02b", Teal: "#179299",
		Sky: "#04a5e5", Blue: "#1e66f5", Lavender: "#7287fd", Mauve: "#8839ef", Pink: "#ea76cb",
		Text: "#4c4f69", Subtext0: "#6c6f85", Overlay2: "#7c7f93", Overlay1: "#8c8fa1",
		Overlay0: "#9ca0b0", Surface1: "#bcc0cc", Base: "#eff1f5",
	},
}

// Names lists the built-in skins.
func Names() []string {
	out := make([]string, 0, len(Builtin))
	for n := range Builtin {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// file is a custom skin: a base built-in and colors over it.
type file struct {
	Base   string  `json:"base,omitempty"`
	Colors Palette `json:"colors"`
}

// Resolve returns the palette for name: a built-in, or dir/<name>.yaml.
// overrides apply last. An empty name is the default.
func Resolve(name, dir string, overrides Palette) (Palette, error) {
	if name == "" {
		name = Default
	}
	p, ok := Builtin[name]
	if !ok {
		path := filepath.Join(dir, name+".yaml")
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return Palette{}, fmt.Errorf("unknown skin %q: not built in (%v) and no %s", name, Names(), path)
		}
		if err != nil {
			return Palette{}, err
		}
		var f file
		if err := yaml.UnmarshalStrict(b, &f); err != nil {
			return Palette{}, fmt.Errorf("%s: %w", path, err)
		}
		if f.Base == "" {
			f.Base = Default
		}
		base, ok := Builtin[f.Base]
		if !ok {
			return Palette{}, fmt.Errorf("%s: base %q is not a built-in skin (%v)", path, f.Base, Names())
		}
		p = merge(base, f.Colors)
	}
	p = merge(p, overrides)
	return p, p.validate()
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (p Palette) validate() error {
	for name, v := range p.fields() {
		if !hexColor.MatchString(*v) {
			return fmt.Errorf("skin color %s = %q: want #rrggbb", name, *v)
		}
	}
	return nil
}

// fields addresses each color by its config name.
func (p *Palette) fields() map[string]*string {
	return map[string]*string{
		"red": &p.Red, "peach": &p.Peach, "yellow": &p.Yellow, "green": &p.Green, "teal": &p.Teal,
		"sky": &p.Sky, "blue": &p.Blue, "lavender": &p.Lavender, "mauve": &p.Mauve, "pink": &p.Pink,
		"text": &p.Text, "subtext0": &p.Subtext0, "overlay2": &p.Overlay2, "overlay1": &p.Overlay1,
		"overlay0": &p.Overlay0, "surface1": &p.Surface1, "base": &p.Base,
	}
}

func merge(base, over Palette) Palette {
	of := over.fields()
	for name, v := range base.fields() {
		if o := *of[name]; o != "" {
			*v = o
		}
	}
	return base
}
