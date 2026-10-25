import { useLayoutEffect, useRef } from "react";
import { Attachments } from "./Attachments";
import { Markdown } from "./Markdown";
import { imagesInText, type MediaRef } from "./media";
import type { Message, Transcript } from "./transcript";
import type { ConnState } from "./useChat";

const THINKING = "nanobot is thinking…";

// attachments are a message's files; a user message's plain text also
// shows the workspace images its markdown names.
function attachments(m: Message): MediaRef[] {
  const media = m.media ?? [];
  if (m.kind !== "user") return media;
  return [...media, ...imagesInText(m.content).filter((i) => !media.some((r) => r.url === i.url))];
}

// Thinking shows a turn's streamed model thinking as plain text in a
// panel that is open while it streams and collapses when it ends; the
// reader can still open it again.
function Thinking({ message }: { message: Message }) {
  const live = !!message.streaming;
  return (
    <details className={"msg thinking" + (live ? " streaming" : "")} open={live}>
      <summary>{live ? "Thinking…" : "Thought"}</summary>
      <div className="thinking-text">{message.content}</div>
    </details>
  );
}

// MessageList shows the chat's messages, the turn's thinking panel, the
// live streaming bubble and, while a reply is due, the pending bubble
// with the turn's progress.
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
      {messages.map((m) =>
        m.kind === "thinking" ? (
          <Thinking key={m.id} message={m} />
        ) : (
          <div key={m.id} className={"msg " + m.kind + (m.streaming ? " streaming" : "")}>
            {m.kind === "assistant" ? <Markdown text={m.content} /> : m.content}
            <Attachments media={attachments(m)} />
          </div>
        ),
      )}
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
