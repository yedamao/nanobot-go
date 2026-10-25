// nanobot-go chat page: one WebSocket to /ws on this origin.
// Client → server: {"type":"chat","content":"..."}, {"type":"stop"}
// Server → client: {"type":"hello","chat_id":"..."},
//                  {"type":"history","messages":[{"role":"user"|"assistant","content":"..."}]},
//                  {"type":"progress","content":"..."},  (transient status)
//                  {"type":"message","role":"assistant","content":"..."},
//                  {"type":"error","content":"..."}
// The sidebar lists stored chats from GET /ui/chats:
//   {"chats":[{"chat_id":"...","updated_at":"...","title":"...","preview":"..."}]}
"use strict";

const CHAT_KEY = "nanobot-go.chatId";
const chatList = document.getElementById("chats");
const toggleChats = document.getElementById("toggle-chats");
const messages = document.getElementById("messages");
const form = document.getElementById("composer");
const input = document.getElementById("input");
const sendButton = document.getElementById("send");
const stopButton = document.getElementById("stop");
const statusEl = document.getElementById("status");
const newChat = document.getElementById("new-chat");

let ws = null;
let retryDelay = 1000;
let retryTimer = null;
let pending = null; // the "thinking" bubble while replies are due
let waiting = 0; // messages sent and not answered yet

const THINKING = "nanobot is thinking…";

function setStatus(state, text) {
  statusEl.dataset.state = state;
  statusEl.textContent = text;
  document.body.dataset.state = state; // drives the empty-state hint
  sendButton.disabled = state !== "open";
  stopButton.disabled = state !== "open";
}

// showHistory replaces the message list with the chat's stored turns.
// The server sends it after every (re)connect, so it also drops anything
// shown before the connection dropped and any reply that raced it.
function showHistory(list) {
  setWaiting(0);
  messages.replaceChildren();
  for (const m of list || []) {
    if (m.role === "user" || m.role === "assistant") append(m.role, m.content);
  }
}

function append(kind, text) {
  const el = document.createElement("div");
  el.className = "msg " + kind;
  el.textContent = text;
  messages.insertBefore(el, pending);
  messages.scrollTop = messages.scrollHeight;
  return el;
}

function setWaiting(n) {
  waiting = Math.max(0, n);
  stopButton.hidden = waiting === 0;
  if (waiting > 0 && !pending) {
    pending = document.createElement("div");
    pending.className = "msg pending";
    pending.textContent = THINKING;
    messages.appendChild(pending);
    messages.scrollTop = messages.scrollHeight;
  } else if (waiting === 0 && pending) {
    pending.remove();
    pending = null;
  }
}

// showProgress puts the running turn's status (such as the tool it runs)
// in the thinking bubble. Progress is never stored: the next message
// frame, or a reconnect's history frame, clears it.
function showProgress(text) {
  // A turn started from another tab of this chat has no bubble here yet.
  if (waiting === 0) setWaiting(1);
  pending.replaceChildren("nanobot is working: ");
  const hint = document.createElement("code");
  hint.textContent = text;
  pending.appendChild(hint);
  messages.scrollTop = messages.scrollHeight;
}

let chats = []; // the last list from GET /ui/chats

// loadChats fetches the stored chats and redraws the sidebar. A failed
// fetch keeps the last list.
async function loadChats() {
  try {
    const resp = await fetch("chats", { cache: "no-store" });
    if (!resp.ok) return;
    chats = (await resp.json()).chats || [];
  } catch {
    return;
  }
  renderChats();
}

// renderChats draws the chat list with the current chat highlighted. A
// chat with nothing stored yet is shown as "New chat" on top.
function renderChats() {
  const active = localStorage.getItem(CHAT_KEY);
  const rows = chats.slice();
  if (!rows.some((c) => c.chat_id === active)) rows.unshift({ chat_id: active, title: "New chat" });
  chatList.replaceChildren(
    ...rows.map((c) => {
      const li = document.createElement("li");
      const item = document.createElement("button");
      item.type = "button";
      item.className = "chat-item";
      if (c.chat_id === active) item.setAttribute("aria-current", "true");
      item.title = c.preview || "";
      item.append(line("chat-title", c.title || c.preview || "Untitled chat"));
      if (c.preview) item.append(line("chat-preview", c.preview));
      if (c.updated_at) item.append(line("chat-time", when(c.updated_at)));
      item.addEventListener("click", () => switchChat(c.chat_id));
      li.appendChild(item);
      return li;
    }),
  );
}

function line(cls, text) {
  const el = document.createElement("span");
  el.className = cls;
  el.textContent = text;
  return el;
}

// when formats a time as a clock time today, or a short date otherwise.
function when(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" });
}

function showSidebar(open) {
  if (open) document.body.dataset.sidebar = "open";
  else delete document.body.dataset.sidebar;
  toggleChats.setAttribute("aria-expanded", String(open));
}

// switchChat reconnects to chat id, or to a new chat when id is null; the
// server's history frame then fills the page.
function switchChat(id) {
  showSidebar(false);
  if (id && id === localStorage.getItem(CHAT_KEY) && ws && ws.readyState === WebSocket.OPEN) return;
  if (id) localStorage.setItem(CHAT_KEY, id);
  else localStorage.removeItem(CHAT_KEY);
  // Clearing now avoids showing the old chat while connecting.
  showHistory([]);
  renderChats();
  const old = ws;
  ws = null;
  if (old) old.close();
  connect();
  input.focus();
}

function connect() {
  clearTimeout(retryTimer);
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const id = localStorage.getItem(CHAT_KEY);
  const url = scheme + "//" + location.host + "/ws" + (id ? "?chat_id=" + encodeURIComponent(id) : "");
  setStatus("connecting", "Connecting…");
  const sock = new WebSocket(url);
  ws = sock;
  sock.onopen = () => {
    retryDelay = 1000;
  };
  sock.onmessage = (ev) => {
    if (ws !== sock) return; // a frame from a socket "New chat" replaced
    let frame;
    try {
      frame = JSON.parse(ev.data);
    } catch {
      return;
    }
    switch (frame.type) {
      case "hello":
        localStorage.setItem(CHAT_KEY, frame.chat_id);
        renderChats();
        break;
      case "history":
        // Sending is enabled only now, so nothing typed meanwhile is
        // wiped by the replaced list.
        showHistory(frame.messages);
        setStatus("open", "Connected");
        loadChats();
        break;
      case "progress":
        showProgress(frame.content || "");
        break;
      case "message":
        setWaiting(waiting - 1);
        if (pending) pending.textContent = THINKING; // the next reply is due
        append("assistant", frame.content);
        loadChats(); // the turn is saved before its reply is sent
        break;
      case "error":
        append("error", frame.content);
        break;
    }
  };
  sock.onclose = () => {
    if (ws !== sock) return; // replaced by "New chat"
    setWaiting(0);
    setStatus("closed", "Disconnected; reconnecting in " + Math.round(retryDelay / 1000) + "s…");
    retryTimer = setTimeout(connect, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10000);
  };
}

function send() {
  const text = input.value.trim();
  if (!text || sendButton.disabled || !ws || ws.readyState !== WebSocket.OPEN) return;
  if (text.toLowerCase() === "/stop") {
    // Not a turn of its own: only the stopped turn is answered.
    ws.send(JSON.stringify({ type: "stop" }));
    input.value = "";
    resize();
    return;
  }
  ws.send(JSON.stringify({ type: "chat", content: text }));
  append("user", text);
  setWaiting(waiting + 1);
  input.value = "";
  resize();
}

function resize() {
  input.style.height = "auto";
  input.style.height = input.scrollHeight + "px";
}

form.addEventListener("submit", (ev) => {
  ev.preventDefault();
  send();
});
input.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
    ev.preventDefault();
    send();
  }
});
input.addEventListener("input", resize);
// The stopped turn is answered "Stopped.", which settles its bubble.
stopButton.addEventListener("click", () => {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "stop" }));
});
newChat.addEventListener("click", () => switchChat(null));
toggleChats.addEventListener("click", () => showSidebar(document.body.dataset.sidebar !== "open"));
// Other tabs may have changed the list meanwhile.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") loadChats();
});

connect();
input.focus();
