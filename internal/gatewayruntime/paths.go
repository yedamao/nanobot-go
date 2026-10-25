// Package gatewayruntime manages a `nanobot gateway` process detached
// from the terminal that started it: a JSON state file with its PID, an
// append-only log file, explicit start/stop/restart, and the client
// leases that let `nanobot webui` share one on-demand gateway and stop it
// when the last client exits.
//
// It mirrors nanobot's nanobot/process_runtime.py and
// nanobot/gateway/runtime.py, simplified (see docs/PORTING.md).
package gatewayruntime

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"

	"github.com/yedamao/nanobot-go/internal/config"
)

// Paths is the filesystem state of one gateway instance.
type Paths struct {
	RunDir, LogsDir string
	// StatePath is run/gateway[.suffix].json: the running gateway's PID,
	// port and launch mode.
	StatePath string
	// LogPath is logs/gateway[.suffix].log: a background gateway's
	// stdout and stderr.
	LogPath string
	// ClientsPath is run/gateway[.suffix].clients.json: the local
	// clients (leases) sharing the gateway and whether it stops when the
	// last one exits.
	ClientsPath string
}

// transitionLock serializes the long lifecycle operations (start, stop,
// restart, lease release); stateLock guards short read-modify-writes of
// the state and clients files. A gateway exiting takes only stateLock,
// so a stop waiting for it never deadlocks.
func (p Paths) transitionLock() string { return p.StatePath + ".transition.lock" }
func (p Paths) stateLock() string      { return p.StatePath + ".lock" }

// Instance is one gateway identity: a config file plus an optional
// workspace override.
type Instance struct {
	ConfigPath string
	Workspace  string // absolute, or empty for the config's own
	Paths      Paths
}

// Resolve returns the instance for configPath and workspace, with its
// state under the config file's directory (run/ and logs/). The default
// config (~/.nanobot-go/config.json) without a workspace override uses
// gateway.json; any other config or a workspace override gets a short
// hash suffix (gateway.<hash>.json) so instances never collide.
func Resolve(configPath, workspace string) (Instance, error) {
	cfg, err := filepath.Abs(configPath)
	if err != nil {
		return Instance{}, err
	}
	ws := ""
	if workspace != "" {
		if workspace, err = config.ExpandHome(workspace); err != nil {
			return Instance{}, err
		}
		if ws, err = filepath.Abs(workspace); err != nil {
			return Instance{}, err
		}
	}
	selector := cfg
	if def, err := config.DefaultPath(); err == nil && filepath.Clean(def) == cfg {
		selector = ""
	}
	return Instance{ConfigPath: cfg, Workspace: ws, Paths: pathsFor(filepath.Dir(cfg), instanceSuffix(ws, selector))}, nil
}

func pathsFor(dataDir, suffix string) Paths {
	stem := "gateway"
	if suffix != "" {
		stem += "." + suffix
	}
	run := filepath.Join(dataDir, "run")
	logs := filepath.Join(dataDir, "logs")
	return Paths{
		RunDir:      run,
		LogsDir:     logs,
		StatePath:   filepath.Join(run, stem+".json"),
		LogPath:     filepath.Join(logs, stem+".log"),
		ClientsPath: filepath.Join(run, stem+".clients.json"),
	}
}

// instanceSuffix is nanobot's _instance_suffix: the first 16 hex digits
// of the SHA-1 of the non-empty selectors joined by "|".
func instanceSuffix(workspace, configPath string) string {
	raw := ""
	for _, v := range []string{workspace, configPath} {
		if v == "" {
			continue
		}
		if raw != "" {
			raw += "|"
		}
		raw += v
	}
	if raw == "" {
		return ""
	}
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}
