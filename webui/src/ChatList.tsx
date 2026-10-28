import { useCallback, useEffect, useState } from "react";
import { deleteChat, listChats, renameChat, type ChatEntry } from "./api";
import { avatarColor, avatarLetter } from "./avatar";
import { useLocale, useT } from "./i18n";

// useChats holds the last list from GET /ui/chats. A failed fetch keeps
// the last list. Other tabs may change it, so it is refetched when the
// tab becomes visible.
export function useChats() {
  const [chats, setChats] = useState<ChatEntry[]>([]);
  const reload = useCallback(async () => {
    try {
      setChats(await listChats());
    } catch {
      // keep the last list
    }
  }, []);
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === "visible") reload();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [reload]);
  return { chats, setChats, reload };
}

// when formats a time as a clock time today, or a short date otherwise.
function when(iso: string, locale: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString(locale, { month: "short", day: "numeric", year: "numeric" });
}

interface Props {
  chats: ChatEntry[];
  active: string | null;
  replyRunning: boolean; // the current chat's reply is still running
  onSelect: (id: string | null) => void;
  onNew: () => void;
  onChanged: () => void; // refetch the list
  onDeleted: (id: string) => void;
}

// ChatList is the sidebar's list. A chat with nothing stored yet is shown
// as "New chat" on top; only stored chats can be renamed or deleted.
export function ChatList({ chats, active, replyRunning, onSelect, onNew, onChanged, onDeleted }: Props) {
  const t = useT();
  const { locale } = useLocale();
  const rows: ChatEntry[] = chats.some((c) => c.chat_id === active)
    ? chats
    : [{ chat_id: active ?? "", title: t("chats.newChat") }, ...chats];

  // busy reports, and tells the user, when id is the current chat and its
  // reply is still running: renaming or deleting it then would race the
  // turn's save.
  const busy = (id: string) => {
    if (id !== active || !replyRunning) return false;
    alert(t("chats.busy"));
    return true;
  };

  const rename = async (c: ChatEntry) => {
    if (busy(c.chat_id)) return;
    const title = prompt(t("chats.renamePrompt"), c.title ?? "");
    if (title === null) return;
    try {
      await renameChat(c.chat_id, title);
    } catch (err) {
      alert(t("chats.renameFailed", { message: (err as Error).message }));
      return;
    }
    onChanged();
  };

  const remove = async (c: ChatEntry) => {
    if (busy(c.chat_id)) return;
    if (!confirm(t("chats.deleteConfirm", { title: c.title || c.preview || t("chats.untitled") }))) return;
    try {
      await deleteChat(c.chat_id);
    } catch (err) {
      alert(t("chats.deleteFailed", { message: (err as Error).message }));
      return;
    }
    onDeleted(c.chat_id);
  };

  return (
    <>
      <button id="new-chat" type="button" title={t("chats.newChatTitle")} onClick={onNew}>
        {t("chats.newChat")}
      </button>
      <ul id="chats">
        {rows.map((c) => (
          <li key={c.chat_id}>
            <button
              type="button"
              className="chat-item"
              aria-current={c.chat_id === active ? "true" : undefined}
              title={c.preview ?? ""}
              onClick={() => onSelect(c.chat_id || null)}
            >
              {c.updated_at ? (
                <span className="chat-avatar" aria-hidden="true" style={{ background: avatarColor(c.chat_id) }}>
                  {avatarLetter(c.title || c.preview)}
                </span>
              ) : (
                <span className="chat-avatar chat-avatar-new" aria-hidden="true">
                  <img src="/favicon.svg" alt="" />
                </span>
              )}
              <span className="chat-text">
                <span className="chat-title">{c.title || c.preview || t("chats.untitled")}</span>
                {c.preview && <span className="chat-preview">{c.preview}</span>}
                {c.updated_at && <span className="chat-time">{when(c.updated_at, locale)}</span>}
              </span>
            </button>
            {c.updated_at && (
              <span className="chat-actions">
                <button type="button" title={t("chats.rename")} aria-label={t("chats.rename")} onClick={() => rename(c)}>
                  ✎
                </button>
                <button type="button" title={t("chats.delete")} aria-label={t("chats.delete")} onClick={() => remove(c)}>
                  ✕
                </button>
              </span>
            )}
          </li>
        ))}
      </ul>
    </>
  );
}
