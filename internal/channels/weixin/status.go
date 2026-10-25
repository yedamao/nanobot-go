package weixin

import (
	"errors"
	"io/fs"
	"strings"
)

// LoginInfo is the non-secret part of a saved login, for the WebUI. It
// never holds the token, the getupdates cursor or context tokens.
type LoginInfo struct {
	// LoggedIn reports a saved bot token.
	LoggedIn bool
	// BotID and UserID are the IDs the login reported.
	BotID, UserID string
	// BaseURL is the API host the login assigned, if any.
	BaseURL string
	// Error describes a state file that cannot be read, without its
	// contents; empty otherwise.
	Error string
}

// LoginStatus reads the saved login at path. A missing file is not
// logged in; an unreadable or corrupt one is not logged in with Error
// set.
func LoginStatus(path string) LoginInfo {
	st, err := stateFile{path: path}.load()
	if err != nil {
		// load's errors may carry parser details; report a fixed message.
		msg := "the saved WeChat login (" + StateFileName + ") is corrupt; fix or remove it, then log in again"
		var perr *fs.PathError
		if errors.As(err, &perr) {
			msg = "the saved WeChat login (" + StateFileName + ") cannot be read"
		}
		return LoginInfo{Error: msg}
	}
	return LoginInfo{
		LoggedIn: st.Token != "",
		BotID:    strings.TrimSpace(st.BotID),
		UserID:   strings.TrimSpace(st.UserID),
		BaseURL:  strings.TrimSpace(st.BaseURL),
	}
}
