package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/yedamao/nanobot-go/internal/pairing"
)

const (
	// PairingPath lists pending and approved senders (GET) and hosts
	// approve/deny/revoke under /ui/pairing/{action}.
	PairingPath = "/ui/pairing"
)

// PairingView is the body of GET /ui/pairing.
type PairingView struct {
	Pending  []PairingPendingView `json:"pending"`
	Approved map[string][]string  `json:"approved"`
}

// PairingPendingView is one pending pairing request.
type PairingPendingView struct {
	Code      string `json:"code"`
	Channel   string `json:"channel"`
	SenderID  string `json:"sender_id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// servePairing answers GET /ui/pairing.
func (c *Channel) servePairing(w http.ResponseWriter, _ *http.Request) {
	pending, err := c.pairing.Pending()
	if err != nil {
		c.log.Warn("websocket: list pairing", "error", err)
		http.Error(w, "cannot list pairing requests", http.StatusInternalServerError)
		return
	}
	approved, err := c.pairing.Approved()
	if err != nil {
		c.log.Warn("websocket: list approved pairing", "error", err)
		http.Error(w, "cannot list approved senders", http.StatusInternalServerError)
		return
	}
	out := PairingView{
		Pending:  make([]PairingPendingView, 0, len(pending)),
		Approved: map[string][]string{},
	}
	for _, p := range pending {
		out.Pending = append(out.Pending, PairingPendingView{
			Code:      p.Code,
			Channel:   p.Channel,
			SenderID:  p.SenderID,
			CreatedAt: p.CreatedAt.UTC().Format(timeRFC3339),
			ExpiresAt: p.ExpiresAt.UTC().Format(timeRFC3339),
		})
	}
	for ch, ids := range approved {
		cp := append([]string(nil), ids...)
		slices.Sort(cp)
		out.Approved[ch] = cp
	}
	writeJSON(w, out)
}

type pairingCodeBody struct {
	Code string `json:"code"`
}

type pairingRevokeBody struct {
	Channel  string `json:"channel"`
	SenderID string `json:"sender_id"`
}

func decodeJSONBody(r *http.Request, dst any, max int64) error {
	if r.Body == nil {
		return io.EOF
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, max))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// approvePairing answers POST /ui/pairing/approve {"code":"..."}.
func (c *Channel) approvePairing(w http.ResponseWriter, r *http.Request) {
	var body pairingCodeBody
	if err := decodeJSONBody(r, &body, 1<<10); err != nil {
		http.Error(w, `body must be {"code":"..."}`, http.StatusBadRequest)
		return
	}
	code := pairing.NormalizeCode(body.Code)
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	req, ok, err := c.pairing.Approve(code)
	if err != nil {
		c.log.Warn("websocket: approve pairing", "error", err)
		http.Error(w, "cannot approve pairing", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "invalid or expired pairing code", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"ok": true,
		"request": PairingPendingView{
			Code:      req.Code,
			Channel:   req.Channel,
			SenderID:  req.SenderID,
			CreatedAt: req.CreatedAt.UTC().Format(timeRFC3339),
			ExpiresAt: req.ExpiresAt.UTC().Format(timeRFC3339),
		},
	})
}

// denyPairing answers POST /ui/pairing/deny {"code":"..."}.
func (c *Channel) denyPairing(w http.ResponseWriter, r *http.Request) {
	var body pairingCodeBody
	if err := decodeJSONBody(r, &body, 1<<10); err != nil {
		http.Error(w, `body must be {"code":"..."}`, http.StatusBadRequest)
		return
	}
	code := pairing.NormalizeCode(body.Code)
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	ok, err := c.pairing.Deny(code)
	if err != nil {
		c.log.Warn("websocket: deny pairing", "error", err)
		http.Error(w, "cannot deny pairing", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "invalid or expired pairing code", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokePairing answers POST /ui/pairing/revoke {"channel","sender_id"}.
func (c *Channel) revokePairing(w http.ResponseWriter, r *http.Request) {
	var body pairingRevokeBody
	if err := decodeJSONBody(r, &body, 1<<10); err != nil {
		http.Error(w, `body must be {"channel":"...","sender_id":"..."}`, http.StatusBadRequest)
		return
	}
	channel := strings.TrimSpace(body.Channel)
	sender := strings.TrimSpace(body.SenderID)
	if channel == "" || sender == "" {
		http.Error(w, "channel and sender_id are required", http.StatusBadRequest)
		return
	}
	ok, err := c.pairing.Revoke(channel, sender)
	if err != nil {
		c.log.Warn("websocket: revoke pairing", "error", err)
		http.Error(w, "cannot revoke pairing", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "sender is not approved", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
