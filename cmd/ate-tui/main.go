// Command ate-tui is a terminal UI for Agent Substrate.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ate-tooling/ate-tui/internal/ateconn"
	"github.com/ate-tooling/ate-tui/internal/config"
	"github.com/ate-tooling/ate-tui/internal/demo"
	"github.com/ate-tooling/ate-tui/internal/skin"
	"github.com/ate-tooling/ate-tui/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

var version = "dev"

func main() {
	var (
		opts     ateconn.Options
		refresh  time.Duration
		atespace string
		filter   string
		skinName string
		demoMode bool
		write    bool
		showVer  bool
	)
	flag.StringVar(&opts.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	flag.StringVar(&opts.Context, "context", "", "kubeconfig context to start on (:ctx switches later)")
	flag.StringVar(&opts.Endpoint, "endpoint", "", "ateapi gRPC endpoint for the starting context; if omitted, port-forwards to the api Service")
	flag.StringVar(&opts.TokenFile, "token-file", "", "bearer token for ateapi on the starting context (defaults to a minted ate-client ServiceAccount token)")
	flag.DurationVar(&refresh, "refresh", 2*time.Second, "poll interval (config: refresh)")
	flag.StringVar(&atespace, "a", "", "atespace to start in (default: all; config: atespace)")
	flag.StringVar(&filter, "filter", "", "initial filter")
	flag.StringVar(&skinName, "skin", "", "color skin: "+fmt.Sprint(skin.Names())+" or a file in the skins directory (config: skin)")
	flag.BoolVar(&demoMode, "demo", false, "run against built-in sample data instead of a cluster")
	flag.BoolVar(&write, "write", false, "allow actions (suspend, resume, revert, delete, drain, mark) and proxies on every context; each still asks to confirm")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: ate-tui [flags] [kind]\n\nkinds: actors, workers, templates, workerpools, atespaces, tags\nconfig: %s\n\n", config.Path())
		flag.PrintDefaults()
	}
	flag.Parse()
	if showVer {
		fmt.Println(version)
		return
	}
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfgPath := config.Path()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fail("config: %v", err)
	}
	if !set["refresh"] {
		if d, _ := cfg.RefreshInterval(); d > 0 {
			refresh = d
		}
	} else if refresh < config.MinRefresh {
		fail("--refresh: %s is below the %s minimum", refresh, config.MinRefresh)
	}
	if !set["a"] {
		atespace = cfg.Atespace
	}
	if !set["skin"] {
		skinName = cfg.Skin
	}
	palette, err := skin.Resolve(skinName, config.SkinDir(), cfg.Colors)
	if err != nil {
		fail("config: %v", err)
	}
	keys, err := ui.ParseKeymap(cfg.Keys)
	if err != nil {
		fail("config: %v", err)
	}
	kind := flag.Arg(0)
	if kind == "" {
		kind = cfg.DefaultView
	}
	if kind != "" && !ui.ValidKind(kind) {
		fail("unknown kind %q", kind)
	}

	label := opts.Context
	if label == "" {
		label = "current context"
	}
	// --endpoint and --token-file belong to the context ate-tui starts on;
	// after a :ctx switch it port-forwards and mints a token as usual.
	startCtx := opts.Context
	if startCtx == "" && !demoMode && (opts.Endpoint != "" || opts.TokenFile != "") {
		cs, _ := ateconn.Contexts(opts.Kubeconfig)
		for _, c := range cs {
			if c.Current {
				startCtx = c.Name
			}
		}
	}
	connect := func(ctx context.Context, name string) (ui.Backend, ui.ConnInfo, error) {
		o := opts
		if name != "" {
			o.Context = name
		}
		if name != "" && name != startCtx {
			o.Endpoint, o.TokenFile = "", ""
		}
		c, err := ateconn.Connect(ctx, o)
		if err != nil {
			return nil, ui.ConnInfo{}, err
		}
		return c, ui.ConnInfo{Context: c.Context, Cluster: c.Host, Endpoint: c.Endpoint}, nil
	}
	contexts := func() ([]ui.KubeContext, error) {
		cs, err := ateconn.Contexts(opts.Kubeconfig)
		if err != nil {
			return nil, err
		}
		out := make([]ui.KubeContext, len(cs))
		for i, c := range cs {
			out[i] = ui.KubeContext{Name: c.Name, Cluster: c.Server, Current: c.Current}
		}
		return out, nil
	}
	if demoMode {
		label = "demo"
		cfg.WriteContexts = nil // the demo writes only with --write
		demoContexts := []ui.KubeContext{
			{Name: "demo", Cluster: "https://substrate.demo:6443", Current: true},
			{Name: "demo-staging", Cluster: "https://staging.substrate.demo:6443"},
		}
		connect = func(_ context.Context, name string) (ui.Backend, ui.ConnInfo, error) {
			for _, c := range demoContexts {
				if c.Name == name || (name == "" && c.Current) {
					return demo.New(), ui.ConnInfo{Context: c.Name, Cluster: c.Cluster}, nil
				}
			}
			return nil, ui.ConnInfo{}, fmt.Errorf("context %q not found", name)
		}
		contexts = func() ([]ui.KubeContext, error) { return demoContexts, nil }
	}

	// client-go reports port-forward and stream errors through klog, which
	// writes to stderr: over the TUI's screen. ate-tui shows the errors it
	// needs itself.
	klog.SetLogger(logr.Discard())

	ui.ApplySkin(palette)
	out := ui.NewOutput(os.Stdout)
	m := ui.New(ui.Options{
		Connect:   connect,
		Contexts:  contexts,
		Context:   opts.Context,
		Refresh:   refresh,
		Version:   version,
		Kind:      kind,
		Atespace:  atespace,
		Filter:    filter,
		ConnLabel: label,

		Write:         write,
		WriteContexts: cfg.WriteContexts,
		ConfigPath:    cfgPath,
		JournalPath:   config.JournalPath(),

		Keys:         keys,
		ProxyAddress: cfg.ProxyAddress,
		Copy:         out.Copy,
	})
	popts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(out)}
	if cfg.MouseEnabled() {
		popts = append(popts, tea.WithMouseCellMotion())
	}
	_, err = tea.NewProgram(m, popts...).Run()
	m.Close()
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ate-tui: "+format+"\n", a...)
	os.Exit(1)
}
