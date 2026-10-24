// nanobot-go chat page: one WebSocket to /ws on this origin.
// Client → server: {"type":"chat","content":"..."}
// Server → client: {"type":"hello","chat_id":"..."},
//                  {"type":"message","role":"assistant","content":"..."},
//                  {"type":"error","content":"..."}
"use strict";

const CHAT_KEY = "nanobot-go.chatId";
const messages = document.getElementById("messages");
const form = document.getElementById("composer");
const input = document.getElementById("input");
const sendButton = document.getElementById("send");
const statusEl = document.getElementById("status");
const newChat = document.getElementById("new-chat");

let ws = null;
let retryDelay = 1000;
let retryTimer = null;
let pending = null; // the "thinking" bubble while replies are due
let waiting = 0; // messages sent and not answered yet

function setStatus(state, text) {
  statusEl.dataset.state = state;
  statusEl.textContent = text;
  sendButton.disabled = state !== "open";
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
  if (waiting > 0 && !pending) {
    pending = document.createElement("div");
    pending.className = "msg pending";
    pending.textContent = "nanobot is thinking…";
    messages.appendChild(pending);
    messages.scrollTop = messages.scrollHeight;
  } else if (waiting === 0 && pending) {
    pending.remove();
    pending = null;
  }
}

function connect() {
  clearTimeout(retryTimer);
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const id = localStorage.getItem(CHAT_KEY);
  const url = scheme + "//" + location.host + "/ws" + (id ? "?chat_id=" + encodeURIComponent(id) : "");
  setStatus("connecting", "connecting…");
  const sock = new WebSocket(url);
  ws = sock;
  sock.onopen = () => {
    retryDelay = 1000;
    setStatus("open", "connected");
  };
  sock.onmessage = (ev) => {
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
      case "message":
        setWaiting(waiting - 1);
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
    setStatus("closed", "disconnected; retrying…");
    retryTimer = setTimeout(connect, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 10000);
  };
}

function send() {
  const text = input.value.trim();
  if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
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
newChat.addEventListener("click", () => {
  localStorage.removeItem(CHAT_KEY);
  setWaiting(0);
  messages.replaceChildren();
  append("info", "New chat started.");
  const old = ws;
  ws = null;
  if (old) old.close();
  connect();
});

connect();
input.focus();
