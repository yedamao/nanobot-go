import { useState, type FormEvent } from "react";
import { login, loginErrorKey } from "./auth";
import { LocaleSwitcher, useT } from "./i18n";

// Login asks for the gateway's access token (channels.websocket's token
// or tokenIssueSecret) and signs in with it.
export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const t = useT();

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (!token.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      const status = await login(token.trim());
      if (status) {
        setError(t(loginErrorKey(status), { status }));
        return;
      }
      setToken("");
      onSignedIn();
    } catch {
      setError(t("login.unreachable"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <main id="login">
      <form id="login-form" onSubmit={submit}>
        <div className="login-title">
          <h1>nanobot-go</h1>
          <LocaleSwitcher id="login-locale" />
        </div>
        <p>{t("login.blurb")}</p>
        <label>
          {t("login.token")}
          <input
            id="login-token"
            type="password"
            autoComplete="current-password"
            autoFocus
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
        </label>
        <p id="login-error" role="alert">
          {error}
        </p>
        <button id="login-submit" type="submit" disabled={busy || !token.trim()}>
          {busy ? t("login.signingIn") : t("login.signIn")}
        </button>
      </form>
    </main>
  );
}
