import { useLayoutEffect, useRef } from "react";
import type { Transcript } from "./transcript";
import type { ConnState } from "./useChat";

const THINKING = "nanobot is thinking…";

// MessageList shows the chat's messages, the live streaming bubble and,
// while a reply is due, the thinking bubble with the turn's progress.
export function MessageList({ transcript, conn }: { transcript: Transcript; conn: ConnState }) {
  const ref = useRef<HTMLElement>(null);
  const { messages, waiting, progress, pendingHidden } = transcript;

  // Keep the newest text in view.
  useLayoutEffect(() => {
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [transcript]);

  const empty = messages.length === 0 && waiting === 0;
  return (
    <main id="messages" ref={ref} aria-live="polite">
      {empty && conn !== "closed" && (
        <p className="empty">
          {conn === "open" ? "No messages yet. Say hello to start the conversation." : "Loading the conversation…"}
        </p>
      )}
      {messages.map((m) => (
        <div key={m.id} className={"msg " + m.kind + (m.streaming ? " streaming" : "")}>
          {m.content}
        </div>
      ))}
      {waiting > 0 && !pendingHidden && (
        <div className="msg pending">
          {progress === null ? (
            THINKING
          ) : (
            <>
              nanobot is working: <code>{progress}</code>
            </>
          )}
        </div>
      )}
    </main>
  );
}
