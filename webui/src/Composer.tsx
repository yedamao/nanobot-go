import { useEffect, useLayoutEffect, useRef, useState, type ChangeEvent, type FormEvent, type KeyboardEvent } from "react";
import { uploadFile, type Upload } from "./api";
import { useT } from "./i18n";
import { attachmentText, type MediaRef } from "./media";
import { formatQuotedUserMessage, quoteSnippet } from "./quote";

interface Props {
  canSend: boolean; // connected and history loaded
  busy: boolean; // a reply is due, so Stop is shown
  onSend: (text: string, media?: MediaRef[]) => boolean;
  onStop: () => void;
  reply: string | null; // the quoted message, if replying
  onCancelReply: () => void;
}

// Composer is the message box: Enter sends, Shift+Enter starts a new line.
// Attach uploads files to the workspace's uploads/ directory; the message
// names them by workspace path, which the agent's file tools open.
// Replying quotes a message ahead of the text, as nanobot's webui does.
export function Composer({ canSend, busy, onSend, onStop, reply, onCancelReply }: Props) {
  const [text, setText] = useState("");
  const [attached, setAttached] = useState<Upload[]>([]);
  const [uploading, setUploading] = useState(0);
  const [uploadError, setUploadError] = useState("");
  const input = useRef<HTMLTextAreaElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  const t = useT();

  // Grow with the text, up to the CSS max-height. (Setting style through
  // the DOM is allowed by the CSP; style attributes in markup are not.)
  useLayoutEffect(() => {
    const el = input.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = el.scrollHeight + "px";
  }, [text]);

  useEffect(() => {
    input.current?.focus();
  }, [reply]);

  const send = () => {
    const t = text.trim();
    if ((!t && attached.length === 0) || !canSend || uploading > 0) return;
    const body = [t, ...attached.map(attachmentText)].filter(Boolean).join("\n\n");
    const media = attached.map(({ url, name, kind }) => ({ url, name, kind }));
    if (onSend(formatQuotedUserMessage(body, reply), media)) {
      setText("");
      setAttached([]);
      setUploadError("");
      onCancelReply();
    }
  };

  const onSubmit = (ev: FormEvent) => {
    ev.preventDefault();
    send();
  };

  const onKeyDown = (ev: KeyboardEvent<HTMLTextAreaElement>) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.nativeEvent.isComposing) {
      ev.preventDefault();
      send();
    }
  };

  const onPick = (ev: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(ev.target.files ?? []);
    ev.target.value = ""; // picking the same file again fires change
    setUploadError("");
    for (const file of files) {
      setUploading((n) => n + 1);
      uploadFile(file)
        .then((up) => setAttached((list) => [...list, up]))
        .catch((err: Error) => setUploadError(file.name + ": " + err.message))
        .finally(() => setUploading((n) => n - 1));
    }
  };

  return (
    <form id="composer" autoComplete="off" onSubmit={onSubmit}>
      {reply !== null && (
        <div className="reply-preview" aria-label={t("composer.replyingTo")}>
          <span className="reply-label">{t("composer.replyingTo")}</span>
          <span className="reply-snippet">{quoteSnippet(reply)}</span>
          <button
            type="button"
            className="remove"
            title={t("composer.cancelReply")}
            aria-label={t("composer.cancelReply")}
            onClick={onCancelReply}
          >
            ×
          </button>
        </div>
      )}
      {(attached.length > 0 || uploading > 0 || uploadError) && (
        <div className="pending-attachments">
          {attached.map((a) => (
            <span key={a.path} className="file-chip">
              {a.name}
              <button
                type="button"
                className="remove"
                title={t("composer.remove", { name: a.name })}
                aria-label={t("composer.remove", { name: a.name })}
                onClick={() => setAttached((list) => list.filter((x) => x.path !== a.path))}
              >
                ×
              </button>
            </span>
          ))}
          {uploading > 0 && <span className="uploading">{t("composer.uploading")}</span>}
          {uploadError && <span className="upload-error">{uploadError}</span>}
        </div>
      )}
      <input ref={picker} type="file" multiple hidden onChange={onPick} />
      <button
        id="attach"
        type="button"
        title={t("composer.attachTitle")}
        aria-label={t("composer.attach")}
        disabled={!canSend}
        onClick={() => picker.current?.click()}
      >
        📎
      </button>
      <textarea
        id="input"
        ref={input}
        rows={1}
        placeholder={t("composer.placeholder")}
        aria-label={t("composer.message")}
        value={text}
        onChange={(ev) => setText(ev.target.value)}
        onKeyDown={onKeyDown}
      />
      {busy && (
        <button id="stop" type="button" title={t("composer.stopTitle")} disabled={!canSend} onClick={onStop}>
          {t("composer.stop")}
        </button>
      )}
      <button id="send" type="submit" disabled={!canSend || uploading > 0}>
        {t("composer.send")}
      </button>
    </form>
  );
}
