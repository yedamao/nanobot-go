// nanobot-go chat page: one WebSocket to /ws on this origin.
// Client → server: {"type":"chat","content":"..."}, {"type":"stop"}
// Server → client: {"type":"hello","chat_id":"..."},
//                  {"type":"history","messages":[{"role":"user"|"assistant","content":"..."}]},
//                  {"type":"progress","content":"..."},  (transient status)
//                  {"type":"delta","content":"..."},  (next piece of the streamed reply)
//                  {"type":"message","role":"assistant","content":"..."},  (the whole reply)
//                  {"type":"error","content":"..."}
// The sidebar lists stored chats from GET /ui/chats:
//   {"chats":[{"chat_id":"...","updated_at":"...","title":"...","preview":"..."}]}
// and renames or deletes one with PATCH /ui/chats/{chat_id} {"title":"..."}
// (answered with the chat's entry) and DELETE /ui/chats/{chat_id} (204).
// The Status panel shows GET /ui/status (read-only):
//   {"version":"...","go":"...","started_at":"...","uptime_seconds":0,"model":"...",
//    "provider":"...","workspace":"...","channels":[{"name":"...","enabled":true,"running":true}],
//    "api_enabled":false,"tools":{"exec":true,"web_fetch":true}}
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
let live = null; // the bubble the reply streams into, until its message frame

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
  live = null;
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
  pending.hidden = false; // under any text streamed so far
  messages.scrollTop = messages.scrollHeight;
}

// showDelta appends a streamed piece of the reply to its bubble, which
// the first piece creates above the thinking bubble. The thinking bubble
// hides while text streams; progress brings it back below the text.
function showDelta(text) {
  if (waiting === 0) setWaiting(1);
  if (!live) live = append("assistant streaming", "");
  live.textContent += text;
  pending.hidden = true;
  messages.scrollTop = messages.scrollHeight;
}

// showReply shows a turn's whole reply. It replaces the streamed text,
// which it supersedes (a stopped turn's partial text becomes "Stopped.").
function showReply(text) {
  if (live) {
    live.textContent = text;
    live.classList.remove("streaming");
    live = null;
  } else {
    append("assistant", text);
  }
  setWaiting(waiting - 1);
  if (pending) {
    pending.textContent = THINKING; // the next reply is due
    pending.hidden = false;
  }
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
      // Only stored chats can be renamed or deleted.
      if (c.updated_at) {
        const actions = document.createElement("span");
        actions.className = "chat-actions";
        actions.append(
          action("✎", "Rename this chat", () => renameChat(c)),
          action("✕", "Delete this chat", () => deleteChat(c)),
        );
        li.appendChild(actions);
      }
      return li;
    }),
  );
}

function action(text, label, onClick) {
  const b = document.createElement("button");
  b.type = "button";
  b.textContent = text;
  b.title = label;
  b.setAttribute("aria-label", label);
  b.addEventListener("click", onClick);
  return b;
}

// busy reports, and tells the user, when chat id is the current chat and
// its reply is still running: renaming or deleting it then would race the
// turn's save.
function busy(id) {
  if (id !== localStorage.getItem(CHAT_KEY) || waiting === 0) return false;
  alert("Wait for the reply to finish, or stop it, first.");
  return true;
}

// chatRequest sends method to /ui/chats/{id}; it returns the response, or
// null after telling the user why it failed.
async function chatRequest(id, method, body, what) {
  let resp;
  try {
    resp = await fetch("chats/" + encodeURIComponent(id), {
      method,
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch (err) {
    alert("Cannot " + what + " the chat: " + err.message);
    return null;
  }
  if (!resp.ok) {
    alert("Cannot " + what + " the chat: " + ((await resp.text()).trim() || "HTTP " + resp.status));
    return null;
  }
  return resp;
}

// renameChat asks for a new title; an empty one goes back to the first
// message.
async function renameChat(c) {
  if (busy(c.chat_id)) return;
  const title = prompt("Rename this chat (leave empty to use its first message):", c.title || "");
  if (title === null) return;
  if (await chatRequest(c.chat_id, "PATCH", { title }, "rename")) loadChats();
}

// deleteChat deletes a chat after confirming; deleting the current chat
// starts a new one, as "New chat" does.
async function deleteChat(c) {
  if (busy(c.chat_id)) return;
  if (!confirm("Delete this chat?\n\n" + (c.title || c.preview || "Untitled chat"))) return;
  if (!(await chatRequest(c.chat_id, "DELETE", null, "delete"))) return;
  chats = chats.filter((x) => x.chat_id !== c.chat_id);
  if (c.chat_id === localStorage.getItem(CHAT_KEY)) switchChat(null);
  else renderChats();
  loadChats();
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

const statusPanel = document.getElementById("status-panel");
const statusList = document.getElementById("status-list");

// loadStatus fetches the gateway status and fills the panel; a failed
// fetch says so in the panel.
async function loadStatus() {
  let st;
  try {
    const resp = await fetch("status", { cache: "no-store" });
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    st = await resp.json();
  } catch (err) {
    statusList.replaceChildren(statusRow("Error", "Cannot load the status (" + err.message + ")."));
    return;
  }
  const onOff = (b) => (b ? "on" : "off");
  const chans = (st.channels || []).map(
    (c) => c.name + " (" + (c.running ? "running" : c.enabled ? "enabled, not running" : "disabled") + ")",
  );
  statusList.replaceChildren(
    statusRow("Version", st.version + (st.go ? " (" + st.go + ")" : "")),
    statusRow("Uptime", duration(st.uptime_seconds || 0)),
    statusRow("Model", st.model || "—"),
    statusRow("Provider", st.provider || "—"),
    statusRow("Workspace", st.workspace || "—"),
    statusRow("Channels", chans.length ? chans.join(", ") : "none"),
    statusRow("API", onOff(st.api_enabled)),
    statusRow("Tools", "exec " + onOff(st.tools && st.tools.exec) + ", web_fetch " + onOff(st.tools && st.tools.web_fetch)),
  );
}

function statusRow(name, value) {
  const row = document.createElement("div");
  const dt = document.createElement("dt");
  dt.textContent = name;
  const dd = document.createElement("dd");
  dd.textContent = value;
  row.append(dt, dd);
  return row;
}

// duration formats seconds as "3d 4h", "2h 5m", "7m 3s" or "9s".
function duration(s) {
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return d + "d " + h + "h";
  if (h) return h + "h " + m + "m";
  if (m) return m + "m " + (s % 60) + "s";
  return s + "s";
}

function showStatus() {
  statusList.replaceChildren(statusRow("Status", "Loading…"));
  statusPanel.showModal();
  loadStatus();
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
      case "delta":
        showDelta(frame.content || "");
        break;
      case "message":
        showReply(frame.content || "");
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
    live = null; // the reconnect's history frame replaces partial text
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
document.getElementById("show-status").addEventListener("click", showStatus);
document.getElementById("refresh-status").addEventListener("click", loadStatus);
document.getElementById("close-status").addEventListener("click", () => statusPanel.close());
// Esc closes the dialog natively.
statusPanel.addEventListener("close", () => input.focus());
// Other tabs may have changed the list meanwhile.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") loadChats();
});

connect();
input.focus();
