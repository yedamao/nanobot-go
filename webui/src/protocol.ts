// The gateway's WebSocket protocol: one socket to /ws on this origin,
// with ?chat_id= to continue a stored chat.
// Client → server: {"type":"chat","content":"...",
//                   "media":[{"url":"/ui/media/...","name":"...","kind":"image"|"file"}]},
//                  {"type":"stop"}
//   (media is optional; the gateway resolves each url to a workspace file
//   and sends images to the model as image parts)
// Server → client: {"type":"hello","chat_id":"..."},
//                  {"type":"history","messages":[{"role":"user"|"assistant","content":"..."}]},
//                  {"type":"progress","content":"..."},  (transient status)
//                  {"type":"delta","content":"..."},  (next piece of the streamed reply)
//                  {"type":"message","role":"assistant","content":"...",
//                   "media":[{"url":"/ui/media/...","name":"...","kind":"image"|"file"}]},  (the whole reply)
//                  {"type":"error","content":"..."}
// Unknown frame types are ignored.

import type { MediaRef } from "./media";

export type Role = "user" | "assistant";

export interface HistoryMessage {
  role: string;
  content: string;
}

export type ServerFrame =
  | { type: "hello"; chat_id: string }
  | { type: "history"; messages?: HistoryMessage[] | null }
  | { type: "progress"; content?: string }
  | { type: "delta"; content?: string }
  | { type: "message"; role?: string; content?: string; media?: unknown }
  | { type: "error"; content?: string };

export type ClientFrame = { type: "chat"; content: string; media?: MediaRef[] } | { type: "stop" };

// chatFrame returns the chat frame for text and its attachments, sending
// only the fields the gateway reads and leaving out an empty media list.
export function chatFrame(content: string, media?: MediaRef[]): ClientFrame {
  if (!media?.length) return { type: "chat", content };
  return { type: "chat", content, media: media.map(({ url, name, kind }) => ({ url, name, kind })) };
}

// parseFrame returns the frame in data, or null when it is not a JSON
// object with a string type.
export function parseFrame(data: unknown): ServerFrame | null {
  if (typeof data !== "string") return null;
  try {
    const v: unknown = JSON.parse(data);
    if (v && typeof v === "object" && typeof (v as { type?: unknown }).type === "string") {
      return v as ServerFrame;
    }
  } catch {
    // not JSON
  }
  return null;
}

// CHAT_KEY is the localStorage key of the current chat id; the plain page
// used the same key, so an open chat survives the upgrade.
export const CHAT_KEY = "nanobot-go.chatId";

export function storedChatId(): string | null {
  return localStorage.getItem(CHAT_KEY);
}

export function storeChatId(id: string | null) {
  if (id) localStorage.setItem(CHAT_KEY, id);
  else localStorage.removeItem(CHAT_KEY);
}

export function socketURL(chatId: string | null): string {
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  return scheme + "//" + location.host + "/ws" + (chatId ? "?chat_id=" + encodeURIComponent(chatId) : "");
}
