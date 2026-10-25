import { useCallback, useEffect, useState } from "react";
import { getAuth, logout, onSignInRequired } from "./auth";
import { ChatList, useChats } from "./ChatList";
import { Login } from "./Login";
import { Composer } from "./Composer";
import { MessageList } from "./MessageList";
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

// App shows the sign-in form when the gateway asks this page to sign in
// (it is reached beyond loopback), and the chat otherwise.
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
      return <Chat onSignOut={signedIn ? signOut : undefined} />;
  }
}

function Chat({ onSignOut }: { onSignOut?: () => void }) {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [statusOpen, setStatusOpen] = useState(false);
  const { chats, setChats, reload } = useChats();
  const { transcript, conn, chatId, send, stop, switchChat } = useChat(reload);

  const select = (id: string | null) => {
    setSidebarOpen(false);
    switchChat(id);
  };

  // Deleting the current chat starts a new one, as New chat does.
  const deleted = (id: string) => {
    setChats((list) => list.filter((c) => c.chat_id !== id));
    if (id === chatId) select(null);
    reload();
  };

  return (
    <div id="app" data-sidebar={sidebarOpen ? "open" : undefined}>
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
      <div id="chat">
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
        <MessageList transcript={transcript} conn={conn.state} />
        <Composer canSend={conn.state === "open"} busy={transcript.waiting > 0} onSend={send} onStop={stop} />
      </div>
      <StatusDialog open={statusOpen} onClose={() => setStatusOpen(false)} />
    </div>
  );
}
