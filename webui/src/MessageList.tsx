import { useLayoutEffect, useRef } from "react";
import { Attachments } from "./Attachments";
import { Markdown } from "./Markdown";
import { imagesInText, type MediaRef } from "./media";
import { parseQuotedUserMessage } from "./quote";
import type { Message, Transcript } from "./transcript";
import type { ConnState } from "./useChat";
import { useT } from "./i18n";

// attachments are a message's files; a user message's plain text (its
// quote aside) also shows the workspace images its markdown names.
function attachments(m: Message, text: string): MediaRef[] {
  const media = m.media ?? [];
  if (m.kind !== "user") return media;
  return [...media, ...imagesInText(text).filter((i) => !media.some((r) => r.url === i.url))];
}

// Thinking shows a turn's streamed model thinking as plain text in a
// panel that is open while it streams and collapses when it ends; the
// reader can still open it again.
function Thinking({ message }: { message: Message }) {
  const t = useT();
  const live = !!message.streaming;
  return (
    <details className={"msg thinking" + (live ? " streaming" : "")} open={live}>
      <summary>{live ? t("messages.thinking") : t("messages.thought")}</summary>
      <div className="thinking-text">{message.content}</div>
    </details>
  );
}

// Bubble shows a user or assistant message; a user message's reply quote
// shows above its text. Reply quotes the message in the composer.
function Bubble({ message: m, onReply }: { message: Message; onReply: (text: string) => void }) {
  const t = useT();
  const { quotedContext, content } = m.kind === "user" ? parseQuotedUserMessage(m.content) : { quotedContext: null, content: m.content };
  const canReply = !m.streaming && (m.kind === "user" || m.kind === "assistant") && content.trim() !== "";
  return (
    <div className={"msg " + m.kind + (m.streaming ? " streaming" : "")}>
      {quotedContext && <blockquote className="msg-quote">{quotedContext}</blockquote>}
      {m.kind === "assistant" ? <Markdown text={content} /> : content}
      <Attachments media={attachments(m, content)} />
      {canReply && (
        <button type="button" className="msg-reply" title={t("messages.replyTitle")} onClick={() => onReply(content)}>
          {t("messages.reply")}
        </button>
      )}
    </div>
  );
}

// MessageList shows the chat's messages, the turn's thinking panel, the
// live streaming bubble and, while a reply is due, the pending bubble
// with the turn's progress.
export function MessageList({
  transcript,
  conn,
  onReply,
}: {
  transcript: Transcript;
  conn: ConnState;
  onReply: (text: string) => void;
}) {
  const t = useT();
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
          {conn === "open" ? t("messages.empty") : t("messages.loading")}
        </p>
      )}
      {messages.map((m) =>
        m.kind === "thinking" ? (
          <Thinking key={m.id} message={m} />
        ) : (
          <Bubble key={m.id} message={m} onReply={onReply} />
        ),
      )}
      {waiting > 0 && !pendingHidden && (
        <div className="msg pending">
          {progress === null ? (
            t("messages.pending")
          ) : (
            <>
              {t("messages.working")} <code>{progress}</code>
            </>
          )}
        </div>
      )}
    </main>
  );
}
