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
	"github.com/ate-tooling/ate-tui/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

func main() {
	var (
		opts     ateconn.Options
		refresh  time.Duration
		atespace string
		filter   string
		demoMode bool
		write    bool
		showVer  bool
	)
	flag.StringVar(&opts.Kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	flag.StringVar(&opts.Context, "context", "", "kubeconfig context to use")
	flag.StringVar(&opts.Endpoint, "endpoint", "", "ateapi gRPC endpoint; if omitted, port-forwards to the api Service")
	flag.StringVar(&opts.TokenFile, "token-file", "", "bearer token for ateapi (defaults to a minted ate-client ServiceAccount token)")
	flag.DurationVar(&refresh, "refresh", 2*time.Second, "poll interval")
	flag.StringVar(&atespace, "a", "", "atespace to start in (default: all)")
	flag.StringVar(&filter, "filter", "", "initial filter")
	flag.BoolVar(&demoMode, "demo", false, "run against built-in sample data instead of a cluster")
	flag.BoolVar(&write, "write", false, "allow actions (suspend, resume, revert, delete, drain, mark); each still asks to confirm")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: ate-tui [flags] [kind]\n\nkinds: actors, workers, templates, workerpools, atespaces, tags\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if showVer {
		fmt.Println(version)
		return
	}

	cfgPath := config.Path()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ate-tui: config:", err)
		os.Exit(1)
	}

	var conn *ateconn.Conn
	label := opts.Context
	if label == "" {
		label = "current context"
	}
	connect := func(ctx context.Context) (ui.Backend, ui.ConnInfo, error) {
		c, err := ateconn.Connect(ctx, opts)
		if err != nil {
			return nil, ui.ConnInfo{}, err
		}
		conn = c
		return c, ui.ConnInfo{Context: c.Context, Cluster: c.Host, Endpoint: c.Endpoint}, nil
	}
	if demoMode {
		label = "demo"
		cfg.WriteContexts = nil // the demo writes only with --write
		connect = func(context.Context) (ui.Backend, ui.ConnInfo, error) {
			return demo.New(), ui.ConnInfo{Context: "demo", Cluster: "https://substrate.demo:6443"}, nil
		}
	}

	m := ui.New(ui.Options{
		Connect:   connect,
		Refresh:   refresh,
		Version:   version,
		Kind:      flag.Arg(0),
		Atespace:  atespace,
		Filter:    filter,
		ConnLabel: label,

		Write:         write,
		WriteContexts: cfg.WriteContexts,
		ConfigPath:    cfgPath,
		JournalPath:   config.JournalPath(),
	})
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	if conn != nil {
		conn.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
