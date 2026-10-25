import { useState } from "react";
import { Composer } from "./Composer";
import { MessageList } from "./MessageList";
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

export default function App() {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const { transcript, conn, send, stop, switchChat } = useChat(() => {});
  const open = conn.state === "open";

  const newChat = () => {
    setSidebarOpen(false);
    switchChat(null);
  };

  return (
    <div id="app" data-sidebar={sidebarOpen ? "open" : undefined}>
      <aside id="sidebar" aria-label="Chats">
        <button id="new-chat" type="button" title="Start a new session" onClick={newChat}>
          New chat
        </button>
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
        </header>
        <MessageList transcript={transcript} conn={conn.state} />
        <Composer canSend={open} busy={transcript.waiting > 0} onSend={send} onStop={stop} />
      </div>
    </div>
  );
}
