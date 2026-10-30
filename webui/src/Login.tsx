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
    <main id="login" className="login-page">
      <form id="login-form" className="login-card" onSubmit={submit}>
        <img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" />
        <h1>nanobot-go</h1>
        <p>{t("login.blurb")}</p>
        <label className="sr-only" htmlFor="login-token">
          {t("login.token")}
        </label>
        <input
          id="login-token"
          className="input mono"
          type="password"
          autoComplete="current-password"
          placeholder={t("login.token")}
          autoFocus
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
        <p id="login-error" role="alert">
          {error}
        </p>
        <button id="login-submit" className="btn primary" type="submit" disabled={busy || !token.trim()}>
          {busy ? t("login.signingIn") : t("login.signIn")}
        </button>
        <div className="foot">
          <span className="host">{location.host}</span>
          <LocaleSwitcher id="login-locale" />
        </div>
      </form>
    </main>
  );
}
