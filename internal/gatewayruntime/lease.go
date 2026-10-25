package gatewayruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// clients is the clients file.
type clients struct {
	// AutoStop marks an on-demand gateway: the last lease released stops
	// it.
	AutoStop bool                    `json:"auto_stop"`
	Clients  map[string]clientRecord `json:"clients"`
}

type clientRecord struct {
	PID      int    `json:"pid"`
	Kind     string `json:"kind"`
	Identity string `json:"identity,omitempty"`
}

// liveClients reads the clients file (state lock held), dropping the
// clients whose process is gone.
func (r *Runtime) liveClients() *clients {
	c := &clients{}
	if data, err := os.ReadFile(r.Paths().ClientsPath); err == nil {
		json.Unmarshal(data, c)
	}
	if c.Clients == nil {
		c.Clients = map[string]clientRecord{}
	}
	for token, rec := range c.Clients {
		if !r.alive(rec.PID) || !r.sameProcess(rec.Identity, rec.PID) {
			delete(c.Clients, token)
		}
	}
	return c
}

// writeClients saves c, or removes the file when it holds nothing.
func (r *Runtime) writeClients(c *clients) error {
	path := r.Paths().ClientsPath
	if len(c.Clients) == 0 && !c.AutoStop {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// ClientLease is one local client (such as `nanobot webui`) of a shared
// gateway. A gateway a lease started on demand stops when the last lease
// is released; an explicit one (`nanobot gateway --background`, or a
// foreground gateway) is never stopped by a lease.
type ClientLease struct {
	rt       *Runtime
	Kind     string
	PID      int
	Token    string
	acquired bool
}

// NewClientLease returns an unacquired lease of kind (such as "webui")
// for the calling process.
func (r *Runtime) NewClientLease(kind string) *ClientLease {
	b := make([]byte, 16)
	rand.Read(b)
	return &ClientLease{rt: r, Kind: kind, PID: r.getpid(), Token: hex.EncodeToString(b)}
}

// Acquire registers the lease before the client starts or attaches to
// the gateway. It waits for a stop in progress to finish.
func (l *ClientLease) Acquire() error {
	p := l.rt.Paths()
	_, err := locked([]string{p.transitionLock(), p.stateLock()}, func() (struct{}, error) {
		c := l.rt.liveClients()
		c.Clients[l.Token] = clientRecord{PID: l.PID, Kind: l.Kind, Identity: l.rt.identity(l.PID)}
		if err := l.rt.writeClients(c); err != nil {
			return struct{}{}, err
		}
		l.acquired = true
		return struct{}{}, nil
	})
	return err
}

// EnsureOnDemand starts the gateway on demand unless it already runs
// (MsgAlreadyRunning).
func (l *ClientLease) EnsureOnDemand(opts StartOptions) (Result, error) {
	if !l.acquired {
		return Result{}, errors.New("gateway client lease must be acquired before startup")
	}
	return l.rt.StartOnDemand(opts)
}

// Release unregisters the lease. When it was the last one of an
// on-demand gateway and stop is set, it stops the gateway (waiting up to
// timeout) and reports whether it did.
func (l *ClientLease) Release(stop bool, timeout time.Duration) (stopped bool, err error) {
	if !l.acquired {
		return false, nil
	}
	p := l.rt.Paths()
	return locked([]string{p.transitionLock()}, func() (bool, error) {
		last, err := locked([]string{p.stateLock()}, func() (bool, error) {
			c := l.rt.liveClients()
			delete(c.Clients, l.Token)
			l.acquired = false
			return len(c.Clients) == 0 && c.AutoStop, l.rt.writeClients(c)
		})
		if err != nil || !last || !stop {
			return false, err
		}
		res, stopErr := l.rt.stopTransition(timeout)
		done := stopErr == nil && (res.OK || res.Message == MsgNotRunning || res.Message == MsgStateStale)
		_, err = locked([]string{p.stateLock()}, func() (struct{}, error) {
			c := l.rt.liveClients()
			if done {
				// Nobody could acquire meanwhile: the transition lock is held.
				c.AutoStop = false
			} else {
				c.AutoStop = true
			}
			return struct{}{}, l.rt.writeClients(c)
		})
		return done, errors.Join(stopErr, err)
	})
}
