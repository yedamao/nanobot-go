import { Fragment, useCallback, useEffect, useState } from "react";
import { deleteChat, listChats, renameChat, type ChatEntry } from "./api";
import { avatarColor, avatarLetter } from "./avatar";
import { Icon } from "./Icon";
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

// when formats a time as a clock time today, a weekday this week, or a
// short date otherwise.
function when(iso: string, locale: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit" });
  }
  if (now.getTime() - d.getTime() < 6 * 86400e3) return d.toLocaleDateString(locale, { weekday: "short" });
  return d.toLocaleDateString(locale, { month: "short", day: "numeric", year: "numeric" });
}

type Group = "today" | "week" | "older";

// groupOf files a chat under Today, Previous 7 days or Older by its last
// update; a chat with nothing stored yet is today's.
function groupOf(iso: string | undefined): Group {
  if (!iso) return "today";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "older";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return "today";
  return now.getTime() - d.getTime() < 7 * 86400e3 ? "week" : "older";
}

const groupLabels = { today: "chats.today", week: "chats.week", older: "chats.older" } as const;

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
// as "New chat" on top; only stored chats can be renamed or deleted. The
// search box filters by title and preview.
export function ChatList({ chats, active, replyRunning, onSelect, onNew, onChanged, onDeleted }: Props) {
  const t = useT();
  const { locale } = useLocale();
  const [query, setQuery] = useState("");
  const all: ChatEntry[] = chats.some((c) => c.chat_id === active)
    ? chats
    : [{ chat_id: active ?? "", title: t("chats.newChat") }, ...chats];
  const q = query.trim().toLocaleLowerCase();
  const rows = q ? all.filter((c) => ((c.title ?? "") + "\n" + (c.preview ?? "")).toLocaleLowerCase().includes(q)) : all;

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

  let lastGroup: Group | null = null;
  return (
    <>
      <button id="new-chat" type="button" className="new-chat" title={t("chats.newChatTitle")} onClick={onNew}>
        <img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" />
        <span>{t("chats.newChat")}</span>
      </button>
      <label className="search">
        <Icon name="search" />
        <input
          type="search"
          placeholder={t("chats.search")}
          aria-label={t("chats.search")}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
      </label>
      <ul id="chats" className="chat-list">
        {rows.length === 0 && <li className="list-empty">{t("chats.noMatch")}</li>}
        {rows.map((c) => {
          const group = groupOf(c.updated_at);
          const label = group !== lastGroup ? <li className="list-label">{t(groupLabels[group])}</li> : null;
          lastGroup = group;
          return (
            <Fragment key={c.chat_id}>
              {label}
              <li className="chat-row">
                <button
                  type="button"
                  className={"chat-item" + (c.chat_id === active ? " active" : "")}
                  aria-current={c.chat_id === active ? "true" : undefined}
                  title={c.preview ?? ""}
                  onClick={() => onSelect(c.chat_id || null)}
                >
                  {c.updated_at ? (
                    <span className="avatar" aria-hidden="true" style={{ background: avatarColor(c.chat_id) }}>
                      {avatarLetter(c.title || c.preview)}
                    </span>
                  ) : (
                    <span className="avatar avatar-new" aria-hidden="true">
                      <img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" />
                    </span>
                  )}
                  <span className="meta">
                    <span className="title">{c.title || c.preview || t("chats.untitled")}</span>
                    {c.preview && <span className="sub">{c.preview}</span>}
                  </span>
                  {c.updated_at && <span className="when">{when(c.updated_at, locale)}</span>}
                </button>
                {c.updated_at && (
                  <span className="chat-actions">
                    <button type="button" className="icon-btn" title={t("chats.rename")} aria-label={t("chats.rename")} onClick={() => rename(c)}>
                      <Icon name="pencil" />
                    </button>
                    <button type="button" className="icon-btn" title={t("chats.delete")} aria-label={t("chats.delete")} onClick={() => remove(c)}>
                      <Icon name="trash" />
                    </button>
                  </span>
                )}
              </li>
            </Fragment>
          );
        })}
      </ul>
    </>
  );
}
