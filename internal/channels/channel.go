// Package channels connects chat platforms to the message bus. It mirrors
// nanobot/channels (base.py, manager.py, registry.py): a Channel publishes
// what it receives as bus.InboundMessage and delivers bus.OutboundMessage;
// the Manager starts the enabled channels, routes outbound messages to
// them by name and stops them. Channels are built from the config's
// "channels" section through a Registry of factories keyed by name.
package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// Channel is one chat platform connection.
type Channel interface {
	// Name is the channel's routing key: InboundMessage.Channel and
	// OutboundMessage.Channel carry it. It matches the registry key and
	// the config section.
	Name() string
	// Start connects and begins listening in the background, publishing
	// received messages to the bus. It returns once the channel is
	// running, or with the reason it cannot run. ctx bounds the start
	// only, not the channel's lifetime.
	Start(ctx context.Context) error
	// Stop stops listening, waits for the channel's goroutines and
	// releases its resources. ctx bounds how long it may take.
	Stop(ctx context.Context) error
	// Send delivers one message. It returns an error when delivery
	// failed; the Manager retries it unless the error is Permanent.
	Send(ctx context.Context, msg bus.OutboundMessage) error
}

// Permanent marks a delivery error that retrying cannot fix (a rejected
// destination, a blocked bot). The Manager does not retry it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// IsPermanent reports whether err, or an error it wraps, came from
// Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Deps is what a factory gets besides its config section.
type Deps struct {
	Bus    *bus.MessageBus
	Logger *slog.Logger
}

// Factory builds a channel from its config section, the raw JSON object
// under channels.<name>. The section includes the "enabled" key; use
// DecodeConfig to parse it strictly.
type Factory func(cfg json.RawMessage, deps Deps) (Channel, error)

// Registry maps channel names to factories. Register every channel
// implementation once at startup; the zero value is not usable.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a factory under name.
func (r *Registry) Register(name string, f Factory) error {
	switch {
	case name == "":
		return errors.New("channels: register: empty name")
	case f == nil:
		return fmt.Errorf("channels: register %s: nil factory", name)
	}
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("channels: register %s: already registered", name)
	}
	r.factories[name] = f
	return nil
}

// Names returns the registered channel names, sorted.
func (r *Registry) Names() []string {
	return slices.Sorted(maps.Keys(r.factories))
}

// Build returns a channel for every section with "enabled": true, in name
// order. An enabled section without a registered factory is an error, so
// a typo in a channel name fails loudly instead of silently running
// without that channel. Disabled and unknown-but-disabled sections are
// skipped.
func (r *Registry) Build(sections map[string]json.RawMessage, deps Deps) ([]Channel, error) {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	var out []Channel
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		raw := sections[name]
		var head struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("channels.%s: %w", name, err)
		}
		if !head.Enabled {
			continue
		}
		f, ok := r.factories[name]
		if !ok {
			return nil, fmt.Errorf("channels.%s: unknown channel (known: %v)", name, r.Names())
		}
		ch, err := f(raw, Deps{Bus: deps.Bus, Logger: deps.Logger.With("channel", name)})
		if err != nil {
			return nil, fmt.Errorf("channels.%s: %w", name, err)
		}
		if ch.Name() != name {
			return nil, fmt.Errorf("channels.%s: factory built a channel named %q", name, ch.Name())
		}
		out = append(out, ch)
	}
	return out, nil
}

// DecodeConfig parses a channel's config section into v, rejecting
// unknown keys like the rest of the config. v should have an Enabled
// field (json "enabled") or the key is rejected too.
func DecodeConfig(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}
