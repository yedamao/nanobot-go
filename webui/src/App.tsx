import { useState } from "react";
import { ChatList, useChats } from "./ChatList";
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
        </header>
        <MessageList transcript={transcript} conn={conn.state} />
        <Composer canSend={conn.state === "open"} busy={transcript.waiting > 0} onSend={send} onStop={stop} />
      </div>
    </div>
  );
}
