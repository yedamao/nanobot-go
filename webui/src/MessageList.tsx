import { useLayoutEffect, useRef, useState } from "react";
import { Attachments } from "./Attachments";
import { Icon } from "./Icon";
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
    <details className={"thinking" + (live ? " streaming" : "")} open={live}>
      <summary>
        <Icon name="sparkle" />
        {live ? t("messages.thinking") : t("messages.thought")}
      </summary>
      <div className="thinking-text">{message.content}</div>
    </details>
  );
}

// MessageActions are a message's hover actions: reply quoting it, copy.
function MessageActions({ content, onReply }: { content: string; onReply: (text: string) => void }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(content).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <div className="msg-actions">
      <button type="button" className="icon-btn msg-reply" title={t("messages.replyTitle")} aria-label={t("messages.reply")} onClick={() => onReply(content)}>
        <Icon name="reply" />
      </button>
      <button type="button" className="icon-btn" title={t(copied ? "messages.copied" : "messages.copy")} aria-label={t("messages.copy")} onClick={copy}>
        <Icon name={copied ? "check" : "copy"} />
      </button>
    </div>
  );
}

const botMark = `${import.meta.env.BASE_URL}favicon.svg`;

// Bubble shows a message: an assistant reply as a full-width document,
// a user message as a compact bubble on the right with its reply quote
// on top. Reply/copy actions sit at the bottom-right of the message.
function Bubble({ message: m, onReply }: { message: Message; onReply: (text: string) => void }) {
  const t = useT();
  const { quotedContext, content } = m.kind === "user" ? parseQuotedUserMessage(m.content) : { quotedContext: null, content: m.content };
  const canReply = !m.streaming && (m.kind === "user" || m.kind === "assistant") && content.trim() !== "";
  const actions = canReply && <MessageActions content={content} onReply={onReply} />;
  if (m.kind === "user") {
    return (
      <div className="msg user">
        <div className="user-col">
          <div className="bubble">
            {quotedContext && (
              <blockquote className="quote">
                <span>{quotedContext}</span>
              </blockquote>
            )}
            {content && <div className="user-text">{content}</div>}
            <Attachments media={attachments(m, content)} />
          </div>
        </div>
        {actions}
      </div>
    );
  }
  if (m.kind === "error") {
    return (
      <div className="msg error" role="alert">
        <Icon name="alert" />
        <div className="body">{content}</div>
      </div>
    );
  }
  return (
    <div className={"msg assistant" + (m.streaming ? " streaming" : "")}>
      <div className="body">
        <div className="who-line">
          <b>{t("messages.bot")}</b>
        </div>
        <div className="prose">
          <Markdown text={content} />
        </div>
        <Attachments media={attachments(m, content)} />
      </div>
      {actions}
    </div>
  );
}

// MessageList shows the chat's messages, the turn's thinking panel, the
// live streaming reply and, while a reply is due, the pending row with
// the turn's progress.
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
    <section id="messages" className="thread" ref={ref} aria-live="polite">
      <div className="thread-inner">
        {empty && conn !== "closed" && (
          <div className="empty">
            <img src={botMark} alt="" />
            <p>{conn === "open" ? t("messages.empty") : t("messages.loading")}</p>
          </div>
        )}
        {messages.map((m) =>
          m.kind === "thinking" ? (
            <div key={m.id} className="msg assistant thinking-row">
              <div className="body">
                <Thinking message={m} />
              </div>
            </div>
          ) : (
            <Bubble key={m.id} message={m} onReply={onReply} />
          ),
        )}
        {waiting > 0 && !pendingHidden && (
          <div className="msg assistant pending">
            <div className="body">
              <div className="who-line">
                <b>{t("messages.bot")}</b>
              </div>
              {progress === null ? (
                <span className="typing" aria-label={t("messages.pending")} title={t("messages.pending")}>
                  <i />
                  <i />
                  <i />
                </span>
              ) : (
                <div className="tool-trace">
                  <span className="spinner" aria-hidden="true" />
                  {t("messages.working")} <code>{progress}</code>
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}
