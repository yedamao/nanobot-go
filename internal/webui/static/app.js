// nanobot-go chat page: one WebSocket to /ws on this origin.
// Client → server: {"type":"chat","content":"..."}, {"type":"stop"}
// Server → client: {"type":"hello","chat_id":"..."},
//                  {"type":"history","messages":[{"role":"user"|"assistant","content":"..."}]},
//                  {"type":"progress","content":"..."},  (transient status)
//                  {"type":"message","role":"assistant","content":"..."},
//                  {"type":"error","content":"..."}
"use strict";

const CHAT_KEY = "nanobot-go.chatId";
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
        break;
      case "history":
        // Sending is enabled only now, so nothing typed meanwhile is
        // wiped by the replaced list.
        showHistory(frame.messages);
        setStatus("open", "Connected");
        break;
      case "progress":
        showProgress(frame.content || "");
        break;
      case "message":
        setWaiting(waiting - 1);
        if (pending) pending.textContent = THINKING; // the next reply is due
        append("assistant", frame.content);
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
newChat.addEventListener("click", () => {
  localStorage.removeItem(CHAT_KEY);
  // The new chat's (empty) history frame clears the list too; clearing
  // now avoids showing the old chat while connecting.
  showHistory([]);
  const old = ws;
  ws = null;
  if (old) old.close();
  connect();
});

connect();
input.focus();
