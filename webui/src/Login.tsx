import { useState, type FormEvent } from "react";
import { login } from "./auth";

// Login asks for the gateway's access token (channels.websocket's token
// or tokenIssueSecret) and signs in with it.
export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (!token.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      const problem = await login(token.trim());
      if (problem) {
        setError(problem);
        return;
      }
      setToken("");
      onSignedIn();
    } catch {
      setError("Cannot reach the gateway.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <main id="login">
      <form id="login-form" onSubmit={submit}>
        <h1>nanobot-go</h1>
        <p>This WebUI is reached over the network, so it needs the access token from the gateway's config (channels.websocket.tokenIssueSecret or token).</p>
        <label>
          Access token
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
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </main>
  );
}
