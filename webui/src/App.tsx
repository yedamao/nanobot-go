import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { getStatus, listMCP, type ChatEntry, type MCPList, type Status } from "./api";
import { getAuth, logout, onSignInRequired } from "./auth";
import { avatarColor, avatarLetter } from "./avatar";
import { ChannelsPage } from "./ChannelsPage";
import { ChatList, useChats } from "./ChatList";
import { Composer } from "./Composer";
import { CronPage } from "./CronPage";
import { Icon, type IconName } from "./Icon";
import { LocaleSwitcher, useT, type MessageKey, type T } from "./i18n";
import { ThemeSwitcher } from "./theme";
import { Login } from "./Login";
import { MCPPage } from "./MCPPage";
import { MessageList } from "./MessageList";
import { ModelsPage } from "./ModelsPage";
import { ProvidersPage } from "./ProvidersPage";
import { RestartButton } from "./RestartGateway";
import { duration } from "./settings";
import { SkillsPage } from "./SkillsPage";
import { StatusPage } from "./StatusPage";
import { useChat, type Connection } from "./useChat";

type Gate = "checking" | "sign-in" | "ready";
type SettingsView = "status" | "channels" | "cron" | "models" | "providers" | "skills" | "mcp";
type View = "chat" | SettingsView;

// The settings shell's nav: groups of pages, each with its icon.
const NAV: { group: MessageKey; items: { id: SettingsView; label: MessageKey; icon: IconName }[] }[] = [
  {
    group: "nav.groups.gateway",
    items: [
      { id: "status", label: "nav.status", icon: "activity" },
      { id: "channels", label: "nav.channels", icon: "radio" },
      { id: "cron", label: "nav.cron", icon: "clock" },
    ],
  },
  {
    group: "nav.groups.intelligence",
    items: [
      { id: "models", label: "nav.models", icon: "sparkle" },
      { id: "providers", label: "nav.providers", icon: "cloud" },
      { id: "skills", label: "nav.skills", icon: "book" },
    ],
  },
  {
    group: "nav.groups.tools",
    items: [{ id: "mcp", label: "nav.mcp", icon: "plug" }],
  },
];

const SETTINGS_VIEWS = NAV.flatMap((g) => g.items.map((i) => i.id));

function readView(): View {
  const h = (typeof location !== "undefined" ? location.hash : "").replace(/^#\/?/, "");
  return (SETTINGS_VIEWS as string[]).includes(h) ? (h as SettingsView) : "chat";
}

function writeView(v: View) {
  const next = v === "chat" ? "#/" : "#/" + v;
  if (location.hash !== next) location.hash = next;
}

const favicon = `${import.meta.env.BASE_URL}favicon.svg`;

// App shows the sign-in form when the gateway asks this page to sign in
// (it is reached beyond loopback), and the workbench otherwise.
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
      return <Shell onSignOut={signedIn ? signOut : undefined} />;
  }
}

// Shell keeps the chat's socket open across views: the chat view and the
// settings shell are two layouts over the same connection.
function Shell({ onSignOut }: { onSignOut?: () => void }) {
  const [view, setView] = useState<View>(readView);
  const chatState = useChats();
  const chat = useChat(chatState.reload);
  const [reply, setReply] = useState<string | null>(null);

  useEffect(() => {
    const onHash = () => setView(readView());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  const go = (v: View) => {
    setView(v);
    writeView(v);
  };

  return view === "chat" ? (
    <ChatView chats={chatState} chat={chat} reply={reply} onReply={setReply} onSignOut={onSignOut} />
  ) : (
    <SettingsShell view={view} conn={chat.conn} onGo={go} />
  );
}

// ConnectionPill shows the chat socket's state: connected, reconnecting
// or offline (with the seconds until the next attempt).
function ConnectionPill({ conn }: { conn: Connection }) {
  const t = useT();
  const state = conn.state === "open" ? "connected" : conn.state === "connecting" ? "connecting" : "offline";
  return (
    <span className="pill conn" data-state={state} role="status">
      <span className="dot" />
      {connectionText(conn, t)}
    </span>
  );
}

function connectionText(c: Connection, t: T): string {
  switch (c.state) {
    case "connecting":
      return t("connection.connecting");
    case "open":
      return t("connection.open");
    case "closed":
      return t("connection.closed", { seconds: c.retryIn });
  }
}

// usePopover opens and closes a menu: a click outside or Esc closes it.
function usePopover() {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (ev: MouseEvent) => {
      if (ref.current && !ref.current.contains(ev.target as Node)) setOpen(false);
    };
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);
  return { open, setOpen, ref };
}

function ChatView({
  chats: { chats, setChats, reload },
  chat: { transcript, conn, chatId, send, stop, switchChat },
  reply,
  onReply: setReply,
  onSignOut,
}: {
  chats: ReturnType<typeof useChats>;
  chat: ReturnType<typeof useChat>;
  reply: string | null;
  onReply: (text: string | null) => void;
  onSignOut?: () => void;
}) {
  const t = useT();
  const [sidebar, setSidebar] = useState<"" | "open" | "collapsed">("");
  const [model, setModel] = useState("");
  const menu = usePopover();

  useEffect(() => {
    getStatus()
      .then((s) => setModel(s.model))
      .catch(() => setModel(""));
  }, []);

  const select = (id: string | null) => {
    setSidebar((s) => (s === "open" ? "" : s));
    setReply(null);
    switchChat(id);
  };

  // Deleting the current chat starts a new one, as New chat does.
  const deleted = (id: string) => {
    setChats((list) => list.filter((c) => c.chat_id !== id));
    if (id === chatId) select(null);
    reload();
  };

  const current: ChatEntry | undefined = chats.find((c) => c.chat_id === chatId);
  const title = current ? current.title || current.preview || t("chats.untitled") : t("chats.newChat");

  return (
    <div className="app" data-sidebar={sidebar || undefined}>
      <aside id="sidebar" className="sidebar" aria-label={t("header.chats")}>
        <div className="brand">
          <img src={favicon} alt="" />
          <div className="brand-name">
            nanobot<span>-go</span>
          </div>
          <button
            type="button"
            className="icon-btn"
            title={t("header.hideChats")}
            aria-label={t("header.hideChats")}
            onClick={() => setSidebar(sidebar === "open" ? "" : "collapsed")}
          >
            <Icon name="panel" />
          </button>
        </div>
        <ChatList
          chats={chats}
          active={chatId}
          replyRunning={transcript.waiting > 0}
          onSelect={select}
          onNew={() => select(null)}
          onChanged={reload}
          onDeleted={deleted}
        />
        <div className="sidebar-foot" ref={menu.ref}>
          <button
            type="button"
            className="user-chip"
            aria-haspopup="menu"
            aria-expanded={menu.open}
            onClick={() => menu.setOpen(!menu.open)}
          >
            <span className="avatar" aria-hidden="true">
              <Icon name={onSignOut ? "key" : "user"} />
            </span>
            <span className="who">
              {t(onSignOut ? "header.signedIn" : "header.local")}
              <small>{location.host}</small>
            </span>
          </button>
          <a className="icon-btn" href="#/status" title={t("header.settings")} aria-label={t("header.settings")}>
            <Icon name="settings" />
          </a>
          {menu.open && (
            <div className="menu user-menu" role="menu">
              <a href="#/status" role="menuitem">
                <Icon name="activity" />
                {t("header.status")}
              </a>
              <a href="#/mcp" role="menuitem">
                <Icon name="settings" />
                {t("header.settings")}
              </a>
              {onSignOut && (
                <button type="button" role="menuitem" title={t("header.changeTokenTitle")} onClick={onSignOut}>
                  <Icon name="key" />
                  {t("header.changeToken")}
                </button>
              )}
              <hr />
              <RestartButton className="menu-restart" label={t("restart.menu")} />
              {onSignOut && (
                <button type="button" role="menuitem" className="danger" title={t("header.signOutTitle")} onClick={onSignOut}>
                  <Icon name="logout" />
                  {t("header.signOut")}
                </button>
              )}
            </div>
          )}
        </div>
      </aside>
      <div className="scrim" onClick={() => setSidebar("")} />
      <main className="main">
        <header className="topbar">
          <button
            type="button"
            id="toggle-chats"
            className="icon-btn"
            title={t("header.chatsTitle")}
            aria-label={t("header.chatsTitle")}
            aria-controls="sidebar"
            onClick={() => setSidebar(sidebar === "collapsed" ? "" : "open")}
          >
            <Icon name="panel" />
          </button>
          <div className="chat-title">
            {current?.updated_at ? (
              <span className="avatar" aria-hidden="true" style={{ background: avatarColor(current.chat_id) }}>
                {avatarLetter(current.title || current.preview)}
              </span>
            ) : (
              <span className="avatar avatar-new" aria-hidden="true">
                <img src={favicon} alt="" />
              </span>
            )}
            <h1>{title}</h1>
          </div>
          <div className="spacer" />
          <ConnectionPill conn={conn} />
          <ThemeSwitcher id="theme" />
          <LocaleSwitcher id="locale" />
          <a className="icon-btn" href="#/status" title={t("header.statusTitle")} aria-label={t("header.statusTitle")}>
            <Icon name="activity" />
          </a>
          <a className="icon-btn" href="#/mcp" title={t("header.settings")} aria-label={t("header.settings")}>
            <Icon name="settings" />
          </a>
        </header>
        <MessageList transcript={transcript} conn={conn.state} onReply={setReply} />
        <Composer
          canSend={conn.state === "open"}
          busy={transcript.waiting > 0}
          onSend={send}
          onStop={stop}
          reply={reply}
          onCancelReply={() => setReply(null)}
          model={model}
        />
      </main>
    </div>
  );
}

// SettingsShell is the one settings product: grouped nav on the left with
// the gateway card pinned under it, a breadcrumb topbar, and the page.
function SettingsShell({ view, conn, onGo }: { view: SettingsView; conn: Connection; onGo: (v: View) => void }) {
  const t = useT();
  const [status, setStatus] = useState<Status | null>(null);
  const [statusFailed, setStatusFailed] = useState(false);
  const [mcp, setMCP] = useState<MCPList | null>(null);
  const [navOpen, setNavOpen] = useState(false);

  // The nav's counts and the gateway card refresh on each page change.
  useEffect(() => {
    getStatus().then(
      (s) => {
        setStatus(s);
        setStatusFailed(false);
      },
      () => setStatusFailed(true),
    );
    listMCP().then(setMCP, () => setMCP(null));
  }, [view]);

  const counts: Partial<Record<SettingsView, ReactNode>> = {};
  if (status?.channels?.length) counts.channels = <span className="count">{status.channels.length}</span>;
  if (mcp) {
    const warn = mcp.restart_required.length + mcp.servers.filter((s) => s.enabled && s.state === "failed").length;
    counts.mcp = warn ? (
      <span className="count warn" title={t("nav.mcpWarn", { count: warn })}>
        {warn} <Icon name="alert" />
      </span>
    ) : (
      <span className="count">{mcp.servers.length || ""}</span>
    );
  }

  const item = NAV.flatMap((g) => g.items).find((i) => i.id === view);
  const port = location.port || (location.protocol === "https:" ? "443" : "80");

  return (
    <div className="settings-app" data-nav={navOpen ? "open" : undefined}>
      <aside className="settings-nav" aria-label={t("nav.settings")}>
        <div className="brand">
          <img src={favicon} alt="" />
          <div className="brand-name">
            nanobot<span>-go</span>
          </div>
        </div>
        <a className="back-link" href="#/" onClick={() => onGo("chat")}>
          <Icon name="arrow-left" />
          {t("nav.backToChat")}
        </a>
        <nav aria-label={t("nav.label")}>
          {NAV.map((g) => (
            <div key={g.group} className="nav-group">
              <div className="list-label">{t(g.group)}</div>
              {g.items.map((i) => (
                <a
                  key={i.id}
                  className={"nav-item" + (view === i.id ? " active" : "")}
                  href={"#/" + i.id}
                  aria-current={view === i.id ? "page" : undefined}
                  onClick={() => setNavOpen(false)}
                >
                  <Icon name={i.icon} />
                  <span>{t(i.label)}</span>
                  {counts[i.id]}
                </a>
              ))}
            </div>
          ))}
        </nav>
        <div className="gateway-card">
          <div className="row">
            <span className={"dot " + (statusFailed ? "err" : status ? "ok" : "")} />
            <b>{t(statusFailed ? "gateway.offline" : "gateway.online")}</b>
          </div>
          {status && (
            <div className="row muted">
              {t("gateway.meta", { version: status.version, uptime: duration(status.uptime_seconds || 0), port })}
            </div>
          )}
          <RestartButton />
        </div>
      </aside>
      <div className="scrim" onClick={() => setNavOpen(false)} />
      <main className="settings-main">
        <header className="topbar">
          <button
            type="button"
            id="toggle-nav"
            className="icon-btn"
            title={t("nav.settings")}
            aria-label={t("nav.settings")}
            onClick={() => setNavOpen(!navOpen)}
          >
            <Icon name="panel" />
          </button>
          <div className="chat-title">
            <span className="crumb">{t("nav.settings")}</span>
            <span className="crumb">/</span>
            <h1>{item ? t(item.label) : ""}</h1>
          </div>
          <div className="spacer" />
          <ConnectionPill conn={conn} />
          <ThemeSwitcher id="theme" />
          <LocaleSwitcher id="locale" />
        </header>
        {view === "status" && <StatusPage />}
        {view === "cron" && <CronPage />}
        {view === "channels" && <ChannelsPage />}
        {view === "models" && <ModelsPage />}
        {view === "providers" && <ProvidersPage />}
        {view === "skills" && <SkillsPage />}
        {view === "mcp" && <MCPPage />}
      </main>
    </div>
  );
}
