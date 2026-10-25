import { useCallback, useEffect, useState } from "react";
import { getAuth, logout, onSignInRequired } from "./auth";
import { ChannelsPage } from "./ChannelsPage";
import { ChatList, useChats } from "./ChatList";
import { Composer } from "./Composer";
import { CronPage } from "./CronPage";
import { Login } from "./Login";
import { MessageList } from "./MessageList";
import { SkillsPage } from "./SkillsPage";
import { StatusDialog } from "./StatusDialog";
import { useChat, type Connection } from "./useChat";

function statusText(c: Connection): string {
  switch (c.state) {
    case "connecting":
      return "Connecting…";
    case "open":
      return "Connected";
    case "closed":
      return "Disconnected; reconnecting in " + c.retryIn + "s…";
  }
}

type Gate = "checking" | "sign-in" | "ready";
type View = "chat" | "cron" | "channels" | "skills";

function readView(): View {
  const h = (typeof location !== "undefined" ? location.hash : "").replace(/^#\/?/, "");
  if (h === "cron" || h === "channels" || h === "skills") return h;
  return "chat";
}

function writeView(v: View) {
  const next = v === "chat" ? "#/" : "#/" + v;
  if (location.hash !== next) location.hash = next;
}

// App shows the sign-in form when the gateway asks this page to sign in
// (it is reached beyond loopback), and the workbench otherwise.
export default function App() {
  const [gate, setGate] = useState<Gate>("checking");
  const [signedIn, setSignedIn] = useState(false);

  const check = useCallback(() => {
    getAuth()
      .then((st) => {
        setSignedIn(st.required && st.authenticated);
        setGate(st.required && !st.authenticated ? "sign-in" : "ready");
      })
      .catch(() => setGate("ready")); // the chat shows the gateway is down
  }, []);

  useEffect(() => {
    check();
    return onSignInRequired(() => setGate("sign-in"));
  }, [check]);

  const signOut = () => {
    void logout().finally(check);
  };

  switch (gate) {
    case "checking":
      return null;
    case "sign-in":
      return <Login onSignedIn={check} />;
    case "ready":
      return <Shell onSignOut={signedIn ? signOut : undefined} />;
  }
}

function Shell({ onSignOut }: { onSignOut?: () => void }) {
  const [view, setView] = useState<View>(readView);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [statusOpen, setStatusOpen] = useState(false);
  const { chats, setChats, reload } = useChats();
  const { transcript, conn, chatId, send, stop, switchChat } = useChat(reload);

  useEffect(() => {
    const onHash = () => setView(readView());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  const go = (v: View) => {
    setView(v);
    writeView(v);
    if (v !== "chat") setSidebarOpen(false);
  };

  const select = (id: string | null) => {
    setSidebarOpen(false);
    go("chat");
    switchChat(id);
  };

  // Deleting the current chat starts a new one, as New chat does.
  const deleted = (id: string) => {
    setChats((list) => list.filter((c) => c.chat_id !== id));
    if (id === chatId) select(null);
    reload();
  };

  return (
    <div id="app" data-sidebar={sidebarOpen ? "open" : undefined} data-view={view}>
      <aside id="sidebar" aria-label="Chats">
        <ChatList
          chats={chats}
          active={chatId}
          replyRunning={transcript.waiting > 0}
          onSelect={select}
          onNew={() => select(null)}
          onChanged={reload}
          onDeleted={deleted}
        />
      </aside>
      <div id="main">
        <header>
          <button
            id="toggle-chats"
            type="button"
            title="Show the chat list"
            aria-controls="sidebar"
            aria-expanded={sidebarOpen}
            onClick={() => setSidebarOpen(!sidebarOpen)}
          >
            Chats
          </button>
          <h1>nanobot-go</h1>
          <nav id="main-nav" aria-label="Workbench">
            {(
              [
                ["chat", "Chat"],
                ["cron", "Cron"],
                ["channels", "Channels"],
                ["skills", "Skills"],
              ] as const
            ).map(([id, label]) => (
              <button
                key={id}
                type="button"
                data-active={view === id || undefined}
                aria-current={view === id ? "page" : undefined}
                onClick={() => go(id)}
              >
                {label}
              </button>
            ))}
          </nav>
          <span className="status" data-state={conn.state} role="status">
            {statusText(conn)}
          </span>
          <button
            id="show-status"
            type="button"
            title="Show the gateway status"
            aria-controls="status-panel"
            onClick={() => setStatusOpen(true)}
          >
            Status
          </button>
          {onSignOut && (
            <button id="sign-out" type="button" title="Sign out of this browser" onClick={onSignOut}>
              Sign out
            </button>
          )}
        </header>
        {view === "chat" && (
          <div id="chat">
            <MessageList transcript={transcript} conn={conn.state} />
            <Composer canSend={conn.state === "open"} busy={transcript.waiting > 0} onSend={send} onStop={stop} />
          </div>
        )}
        {view === "cron" && <CronPage />}
        {view === "channels" && <ChannelsPage />}
        {view === "skills" && <SkillsPage />}
      </div>
      <StatusDialog open={statusOpen} onClose={() => setStatusOpen(false)} />
    </div>
  );
}
