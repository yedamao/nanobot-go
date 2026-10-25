package weixin

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// stateLockFile serializes updates of account.json between the gateway
// and `nanobot channels login weixin`.
const stateLockFile = ".account.lock"

// accountState is account.json, in nanobot's layout (state.py and
// WeixinChannel._save_state) plus the IDs the login reported. It holds
// the bot token, so it is written with mode 0600 in a 0700 directory.
type accountState struct {
	Token string `json:"token"`
	// GetUpdatesBuf is the getupdates cursor.
	GetUpdatesBuf string `json:"get_updates_buf"`
	// ContextTokens maps a user ID to the latest context_token, which
	// every reply to that user needs.
	ContextTokens map[string]string `json:"context_tokens"`
	// BaseURL is the API host the login assigned to the account.
	BaseURL string `json:"base_url"`
	// ReplacedConfigTokenSHA256 is set when a login replaced a token
	// configured in channels.weixin.token (or the environment): the
	// SHA-256 of that token. While the configured token is unchanged, the
	// saved token wins over it.
	ReplacedConfigTokenSHA256 string `json:"replaced_config_token_sha256,omitempty"`
	BotID                     string `json:"ilink_bot_id,omitempty"`
	UserID                    string `json:"ilink_user_id,omitempty"`
}

// LoggedIn reports whether the account.json at path holds a token from
// `nanobot channels login weixin`. An unreadable file is not logged in.
func LoggedIn(path string) bool {
	st, err := stateFile{path: path}.load()
	return err == nil && st.Token != ""
}

// stateFile is account.json and its lock.
type stateFile struct{ path string }

// load reads the state. A missing or empty file is the zero state; one
// that does not parse is an error.
func (s stateFile) load() (accountState, error) {
	var st accountState
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("weixin state: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, &st); err != nil {
		// The JSON error never quotes the file's contents.
		return accountState{}, fmt.Errorf("weixin state %s is corrupt (fix or remove it, then log in again): %w", s.path, err)
	}
	st.Token = strings.TrimSpace(st.Token)
	return st, nil
}

// update runs fn on the state under the file lock and saves it when fn
// reports a change.
func (s stateFile) update(fn func(*accountState) bool) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if !fn(&st) {
		return nil
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("weixin state: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("weixin state: %w", err)
	}
	return nil
}

func (s stateFile) lock() (func(), error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("weixin state: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, stateLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("weixin state: %w", err)
	}
	if err := fsutil.LockExclusive(f, true); err != nil {
		f.Close()
		return nil, fmt.Errorf("weixin state: lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

// HasSavedLogin reports whether the state at path holds a bot token, for
// `nanobot status`. It never returns the token.
func HasSavedLogin(path string) (bool, error) {
	st, err := stateFile{path}.load()
	return st.Token != "", err
}

// tokenFingerprint is the SHA-256 of a token, as stored in
// replaced_config_token_sha256.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// account is the token the channel uses and where it came from.
type account struct {
	token, baseURL string
	// fromState is true when the token is the saved one, false when it is
	// the configured one.
	fromState bool
	cursor    string
	contexts  map[string]string
}

// chooseAccount picks the token like nanobot's start: a configured token
// wins, unless a login has since replaced exactly that token; otherwise
// the saved one. The cursor and context tokens are restored only for the
// token they were saved with. ok is false when there is no token at all.
func chooseAccount(cfg Config, st accountState) (account, bool) {
	saved := account{token: st.Token, baseURL: cmp.Or(st.BaseURL, cfg.BaseURL), fromState: true,
		cursor: st.GetUpdatesBuf, contexts: st.ContextTokens}
	switch {
	case cfg.Token == "":
		return saved, st.Token != ""
	case st.Token != "" && st.Token != cfg.Token && st.ReplacedConfigTokenSHA256 == tokenFingerprint(cfg.Token):
		return saved, true
	}
	a := account{token: cfg.Token, baseURL: cfg.BaseURL}
	if st.Token == cfg.Token {
		a.cursor, a.contexts = st.GetUpdatesBuf, st.ContextTokens
		a.baseURL = cmp.Or(st.BaseURL, cfg.BaseURL)
	}
	return a, true
}
