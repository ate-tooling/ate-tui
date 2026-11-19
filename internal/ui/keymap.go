package ui

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// defaultKeys are the table keys config.yaml can rebind, by action name.
// Pager, dialog and text-entry keys are fixed.
var defaultKeys = []struct{ name, key string }{
	{"quit", "q"}, {"down", "j"}, {"up", "k"}, {"top", "g"}, {"bottom", "G"},
	{"page-down", "ctrl+f"}, {"page-up", "ctrl+b"}, {"filter", "/"}, {"command", ":"}, {"help", "?"},
	{"back", "["}, {"forward", "]"}, {"next-view", "tab"}, {"prev-view", "shift+tab"},
	{"all-atespaces", "0"}, {"sort", "S"}, {"invert", "I"}, {"faults", "ctrl+z"}, {"mark", " "},
	{"refresh", "r"}, {"wide", "w"}, {"copy", "c"}, {"create", "n"}, {"clone", "ctrl+n"}, {"edit", "ctrl+e"},
	{"explain", "X"}, {"yaml", "y"}, {"relations", "u"}, {"logs", "l"}, {"proxy", "f"},
	{"egress", "e"}, {"events", "E"}, {"timeline", "T"}, {"worker", "o"}, {"template", "t"}, {"pool", "p"},
	{"suspend", "s"}, {"resume", "R"}, {"revert", "v"}, {"delete", "ctrl+d"},
	{"drain", "D"}, {"available", "A"}, {"unavailable", "U"}, {"scale", "z"},
}

// reservedKeys keep their meaning whatever the config says.
var reservedKeys = map[string]bool{
	"enter": true, "esc": true, "ctrl+c": true, "up": true, "down": true,
	"home": true, "end": true, "pgup": true, "pgdown": true,
}

// Keymap translates rebound keys to the defaults the handlers switch on.
type Keymap struct {
	to    map[string]string // pressed key → default key
	moved map[string]bool   // default keys bound to something else now
	shown map[string]string // default key → key to show
	names map[string]string // action → key, as configured
}

// ParseKeymap reads the config's keys: action name → key. Keys use Bubble
// Tea names ("ctrl+x", "alt+x", "F"), and "space" for the space bar.
func ParseKeymap(cfg map[string]string) (Keymap, error) {
	km := Keymap{to: map[string]string{}, moved: map[string]bool{}, shown: map[string]string{}, names: map[string]string{}}
	defaults := map[string]string{}
	for _, d := range defaultKeys {
		defaults[d.name] = d.key
	}
	for name, key := range cfg {
		def, ok := defaults[name]
		if !ok {
			return Keymap{}, fmt.Errorf("keys: unknown action %q", name)
		}
		key = normalizeKey(key)
		if err := validKey(key); err != nil {
			return Keymap{}, fmt.Errorf("keys: %s: %w", name, err)
		}
		km.names[name] = key
		if key == def {
			continue
		}
		km.to[key], km.moved[def], km.shown[def] = def, true, key
	}
	// Every action must end up on its own key.
	owner := map[string]string{}
	for _, d := range defaultKeys {
		key := d.key
		if k, ok := km.names[d.name]; ok {
			key = k
		}
		if o, dup := owner[key]; dup {
			return Keymap{}, fmt.Errorf("keys: %q is bound to both %s and %s", displayKey(key), o, d.name)
		}
		owner[key] = d.name
	}
	return km, nil
}

func normalizeKey(k string) string {
	if strings.EqualFold(k, "space") {
		return " "
	}
	return k
}

func validKey(k string) error {
	switch {
	case k == "":
		return fmt.Errorf("empty key")
	case reservedKeys[k]:
		return fmt.Errorf("%q is reserved", k)
	case utf8.RuneCountInString(k) == 1:
		return nil
	}
	mod, rest, ok := strings.Cut(k, "+")
	if ok && (mod == "ctrl" || mod == "alt" || mod == "shift") && rest != "" {
		return nil
	}
	if k == "tab" || (len(k) >= 2 && k[0] == 'f' && strings.Trim(k[1:], "0123456789") == "") {
		return nil
	}
	return fmt.Errorf("%q is not a key; use one character, ctrl+x, alt+x, tab or f1-f12", k)
}

// translate maps a pressed key to the default it stands for. A default key
// that was rebound elsewhere does nothing.
func (k Keymap) translate(key string) (string, bool) {
	if def, ok := k.to[key]; ok {
		return def, true
	}
	if k.moved[key] {
		return "", false
	}
	return key, true
}

// show is the key to display for a default key, or for "[ ]" a pair.
func (k Keymap) show(def string) string {
	if def != " " && strings.Contains(def, " ") {
		parts := strings.Fields(def)
		for i, p := range parts {
			parts[i] = k.show(p)
		}
		return strings.Join(parts, " ")
	}
	if s, ok := k.shown[def]; ok {
		return displayKey(s)
	}
	return displayKey(def)
}

// custom lists the rebound actions for the help page.
func (k Keymap) custom() []hint {
	var out []hint
	for name, key := range k.names {
		out = append(out, hint{displayKey(key), name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].desc < out[j].desc })
	return out
}

func displayKey(k string) string {
	switch k {
	case " ":
		return "␣"
	case "enter":
		return "⏎"
	}
	if rest, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return "^" + rest
	}
	return k
}
