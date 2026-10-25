// Sign-in for browsers beyond the gateway's own device. GET /ui/auth says
// whether this page must sign in; POST /ui/login {"token":"..."} takes the
// configured token or tokenIssueSecret and sets an httpOnly session
// cookie, which later requests and the WebSocket send on their own. The
// secret is never kept in the page.

export interface AuthState {
  required: boolean;
  authenticated: boolean;
}

export async function getAuth(): Promise<AuthState> {
  const resp = await fetch("auth", { cache: "no-store" });
  if (!resp.ok) throw new Error("HTTP " + resp.status);
  return resp.json();
}

// login signs in with token; it returns "" on success or why it failed.
export async function login(token: string): Promise<string> {
  const resp = await fetch("login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
  return resp.ok ? "" : loginError(resp.status);
}

export async function logout(): Promise<void> {
  await fetch("logout", { method: "POST" });
}

export function loginError(status: number): string {
  switch (status) {
    case 401:
      return "That access token is not right.";
    case 429:
      return "Too many failed attempts; wait a few minutes and try again.";
    case 404:
      return "Sign-in is not configured on this gateway.";
    default:
      return "Sign-in failed (HTTP " + status + ").";
  }
}

// A 401 from any /ui/ endpoint means the session ended (or never began);
// the app listens for this event and shows the sign-in form.
const signInEvent = "nanobot-sign-in-required";

export function signInRequired(): void {
  window.dispatchEvent(new Event(signInEvent));
}

export function onSignInRequired(fn: () => void): () => void {
  window.addEventListener(signInEvent, fn);
  return () => window.removeEventListener(signInEvent, fn);
}

// checkSignedIn asks the gateway after the WebSocket closed, since a
// refused upgrade does not tell the page why; it signals when the page
// must sign in again.
export async function checkSignedIn(): Promise<void> {
  try {
    const st = await getAuth();
    if (st.required && !st.authenticated) signInRequired();
  } catch {
    // The gateway is down; the socket keeps retrying.
  }
}
