package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/yedamao/wally/internal/channels/weixin"
	"github.com/yedamao/wally/internal/config"
)

const channelsUsage = `Usage:
  wally channels login weixin[.<name>] [--force] [--config path]

login weixin[.<name>]  log in to a personal WeChat instance (Tencent's iLink bot API): prints
              the link of a QR code to scan with WeChat and waits for the
              confirmation, then saves the bot token to
              state/<instance>/account.json next to the config file (mode
              0600; channels.<instance>.stateDir moves it). --force logs in
              again even when a login is saved. The gateway's WeChat
              channel uses the saved login.
`

// channelsCmd runs the chat channels' interactive commands.
func (a *app) channelsCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(a.stderr, channelsUsage)
		if len(args) == 0 {
			return errUsage
		}
		return flag.ErrHelp
	}
	if args[0] != "login" {
		fmt.Fprintf(a.stderr, "wally channels: unknown subcommand %q\n\n%s", args[0], channelsUsage)
		return errUsage
	}
	return a.channelsLogin(ctx, args[1:])
}

func (a *app) channelsLogin(ctx context.Context, args []string) error {
	fs := a.newFlagSet("channels login", "channels login weixin[.<name>] [--force] [--config path]")
	configFlag := fs.String("config", "", "config file (default $WALLY_CONFIG or ~/.wally/config.json; legacy $NANOBOT_GO_CONFIG supported)")
	force := fs.Bool("force", false, "log in again even when a login is saved")
	// Accept the channel name before or after the flags.
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprintf(fs.Output(), "want the channel name, got %q\n", pos)
		fs.Usage()
		return errUsage
	}
	instance := pos[0]
	if !weixin.IsName(instance) {
		return fmt.Errorf("channels login: %q has no login (only weixin or weixin.<name> does)", instance)
	}
	path, err := config.ResolvePath(*configFlag, a.getenv)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	raw := cfg.Channels[instance]
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	wcfg, err := weixin.ParseConfig(raw, a.getenv)
	if err != nil {
		return fmt.Errorf("channels.%s: %w", instance, err)
	}
	statePath := wcfg.StatePath(weixin.DefaultStateDir(path, instance))

	// Ctrl-C cancels the login (signals reach the CLI as a.interrupts).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.interrupts:
			cancel()
		case <-ctx.Done():
		}
	}()
	res, err := weixin.Login(ctx, weixin.LoginOptions{
		Config:    wcfg,
		StatePath: statePath,
		Force:     *force,
		Out:       a.stdout,
		In:        a.stdin,
		Version:   version,
	})
	if err != nil {
		return err
	}
	if res.AlreadyLoggedIn {
		fmt.Fprintf(a.stdout, "WeChat is already logged in (%s). Use --force to log in again.\n", statePath)
		return nil
	}
	fmt.Fprintf(a.stdout, "Logged in to WeChat")
	if res.BotID != "" {
		fmt.Fprintf(a.stdout, " as bot %s", res.BotID)
	}
	fmt.Fprintf(a.stdout, ". Saved to %s.\n", statePath)
	if !rawEnabled(raw) {
		fmt.Fprintf(a.stdout, "Set channels.%s.enabled to true in %s, then start 'nanobot gateway'.\n", instance, path)
	} else {
		fmt.Fprintln(a.stdout, "Start (or restart) 'nanobot gateway' to use it; a gateway whose login expired picks it up by itself.")
	}
	return nil
}

// rawEnabled reports whether a channel section has "enabled": true.
func rawEnabled(raw json.RawMessage) bool {
	var head struct {
		Enabled bool `json:"enabled"`
	}
	return json.Unmarshal(raw, &head) == nil && head.Enabled
}
