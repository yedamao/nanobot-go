package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/pairing"
)

const pairingUsage = `Usage:
  nanobot pairing list [--config path]
  nanobot pairing approve <code> [--config path]
  nanobot pairing deny <code> [--config path]
  nanobot pairing revoke <channel> <sender_id> [--config path]

A sender a chat channel does not allow (allowFrom) who writes to the bot
in a private chat gets a pairing code, valid for 10 minutes. Approving the
code lets that sender talk to the bot from then on. The approvals live in
pairing.json next to the config file; no API key is needed.
`

// pairingCmd manages the senders approved through pairing codes.
func (a *app) pairingCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(a.stderr, pairingUsage)
		if len(args) == 0 {
			return errUsage
		}
		return flag.ErrHelp
	}
	switch args[0] {
	case "list", "ls":
		return a.pairingList(args[1:])
	case "approve":
		return a.pairingApprove(args[1:])
	case "deny":
		return a.pairingDeny(args[1:])
	case "revoke":
		return a.pairingRevoke(args[1:])
	}
	fmt.Fprintf(a.stderr, "nanobot pairing: unknown subcommand %q\n\n%s", args[0], pairingUsage)
	return errUsage
}

// pairingFlags parses --config around exactly n positional arguments and
// opens the pairing store of that config.
func (a *app) pairingFlags(name, synopsis string, args []string, n int) (*pairing.Store, []string, error) {
	fs := a.newFlagSet(name, synopsis)
	configFlag := fs.String("config", "", "config file (default $NANOBOT_GO_CONFIG or ~/.nanobot-go/config.json)")
	// Accept the positionals before or after the flags.
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil, err
		}
		return nil, nil, errUsage
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != n {
		fmt.Fprintf(fs.Output(), "want %d argument(s), got %d\n", n, len(pos))
		fs.Usage()
		return nil, nil, errUsage
	}
	path, err := config.ResolvePath(*configFlag, a.getenv)
	if err != nil {
		return nil, nil, err
	}
	return pairing.NewStore(pairing.PathFor(path)), pos, nil
}

func (a *app) pairingList(args []string) error {
	store, _, err := a.pairingFlags("pairing list", "pairing list [--config path]", args, 0)
	if err != nil {
		return err
	}
	pending, err := store.Pending()
	if err != nil {
		return err
	}
	approved, err := store.Approved()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	if len(pending) == 0 {
		fmt.Fprintln(w, "No pending pairing requests.")
	} else {
		fmt.Fprintln(w, "CODE\tCHANNEL\tSENDER\tEXPIRES IN")
		for _, p := range pending {
			left := max(time.Until(p.ExpiresAt).Round(time.Second), 0)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Code, p.Channel, p.SenderID, left)
		}
	}
	if len(approved) > 0 {
		fmt.Fprintln(w, "\nCHANNEL\tAPPROVED SENDERS")
		for _, ch := range slices.Sorted(maps.Keys(approved)) {
			fmt.Fprintf(w, "%s\t%s\n", ch, strings.Join(approved[ch], ", "))
		}
	}
	return w.Flush()
}

func (a *app) pairingApprove(args []string) error {
	store, pos, err := a.pairingFlags("pairing approve", "pairing approve <code> [--config path]", args, 1)
	if err != nil {
		return err
	}
	req, ok, err := store.Approve(pos[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invalid or expired pairing code %s", pairing.NormalizeCode(pos[0]))
	}
	fmt.Fprintf(a.stdout, "Approved %s: sender %s can now use %s.\n", req.Code, req.SenderID, req.Channel)
	return nil
}

func (a *app) pairingDeny(args []string) error {
	store, pos, err := a.pairingFlags("pairing deny", "pairing deny <code> [--config path]", args, 1)
	if err != nil {
		return err
	}
	ok, err := store.Deny(pos[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pairing code %s not found or already expired", pairing.NormalizeCode(pos[0]))
	}
	fmt.Fprintf(a.stdout, "Denied %s.\n", pairing.NormalizeCode(pos[0]))
	return nil
}

func (a *app) pairingRevoke(args []string) error {
	store, pos, err := a.pairingFlags("pairing revoke", "pairing revoke <channel> <sender_id> [--config path]", args, 2)
	if err != nil {
		return err
	}
	ok, err := store.Revoke(pos[0], pos[1])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not an approved sender of %s", pos[1], pos[0])
	}
	fmt.Fprintf(a.stdout, "Revoked %s from %s.\n", pos[1], pos[0])
	return nil
}
