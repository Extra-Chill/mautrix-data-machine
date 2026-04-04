package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"go.mau.fi/mautrix-datamachine/pkg/bot"
	"go.mau.fi/mautrix-datamachine/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	// Check for --mode=bot before any flag parsing, since bridge mode
	// uses mauflag which would reject unknown flags.
	if isBotMode() {
		if err := runBot(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Default: bridge mode. The mxmain framework handles its own flags.
	runBridge()
}

// isBotMode checks os.Args for --mode=bot or --mode bot.
func isBotMode() bool {
	for i, arg := range os.Args[1:] {
		if arg == "--mode=bot" || arg == "-mode=bot" {
			return true
		}
		if (arg == "--mode" || arg == "-mode") && i+2 < len(os.Args) && os.Args[i+2] == "bot" {
			return true
		}
	}
	return false
}

// stripModeFlag removes --mode and its value from os.Args so flag.Parse
// in runBot doesn't see it as a leftover (it's already been consumed).
func stripModeFlag() []string {
	var filtered []string
	skip := false
	for _, arg := range os.Args[1:] {
		if skip {
			skip = false
			continue
		}
		if arg == "--mode=bot" || arg == "-mode=bot" || arg == "--mode=bridge" || arg == "-mode=bridge" {
			continue
		}
		if arg == "--mode" || arg == "-mode" {
			skip = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func runBridge() {
	c := &connector.DataMachineConnector{}
	m := mxmain.BridgeMain{
		Name:        "mautrix-datamachine",
		Description: "A Matrix-Data Machine chat bridge for Beeper",
		URL:         "https://github.com/Extra-Chill/mautrix-data-machine",
		Version:     "0.1.0",
		SemCalVer:   false,
		Connector:   c,
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}

func runBot() error {
	// Parse bot-specific flags from cleaned args.
	fs := flag.NewFlagSet("mautrix-datamachine-bot", flag.ExitOnError)
	configPath := fs.String("c", "", "Path to bot config file (YAML)")
	if err := fs.Parse(stripModeFlag()); err != nil {
		return err
	}

	if *configPath == "" {
		return fmt.Errorf("config file is required for bot mode (use -c flag)")
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var cfg bot.BotConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	b := &bot.Bot{Config: cfg}
	return b.Run(ctx)
}
