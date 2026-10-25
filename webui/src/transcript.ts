import type { MediaRef } from "./media";
import type { HistoryMessage } from "./protocol";

export interface Message {
  id: number;
  kind: "user" | "assistant" | "error";
  content: string;
  streaming?: boolean; // the live bubble deltas append to
  media?: MediaRef[]; // files shown under the bubble
}

// Transcript is what the page shows of the current chat.
export interface Transcript {
  messages: Message[];
  waiting: number; // messages sent and not answered yet
  live: number | null; // id of the streaming bubble, until its message frame
  progress: string | null; // the running turn's status, such as its tool
  pendingHidden: boolean; // the thinking bubble hides while text streams
  nextId: number;
}

export type Action =
  | { type: "history"; messages: HistoryMessage[] }
  | { type: "sent"; content: string; media?: MediaRef[] }
  | { type: "progress"; content: string }
  | { type: "delta"; content: string }
  | { type: "message"; content: string; media?: MediaRef[] }
  | { type: "error"; content: string }
  | { type: "disconnected" };

export const emptyTranscript: Transcript = {
  messages: [],
  waiting: 0,
  live: null,
  progress: null,
  pendingHidden: false,
  nextId: 1,
};

function push(t: Transcript, m: Omit<Message, "id">): Transcript {
  return { ...t, messages: [...t.messages, { ...m, id: t.nextId }], nextId: t.nextId + 1 };
}

// A frame for a turn started from another tab of this chat arrives with
// nothing sent here; it counts as one reply due.
function due(t: Transcript): number {
  return t.waiting === 0 ? 1 : t.waiting;
}

export function reduce(t: Transcript, a: Action): Transcript {
  switch (a.type) {
    case "history": {
      // The server sends history after every (re)connect, so it replaces
      // anything shown before, including a reply that raced it.
      let next: Transcript = { ...emptyTranscript, nextId: t.nextId };
      for (const m of a.messages) {
        if (m.role === "user" || m.role === "assistant") next = push(next, { kind: m.role, content: m.content });
      }
      return next;
    }
    case "sent":
      return {
        ...push(t, { kind: "user", content: a.content, media: a.media?.length ? a.media : undefined }),
        waiting: t.waiting + 1,
      };
    case "progress":
      // Progress is never stored: the next message or history clears it.
      return { ...t, waiting: due(t), progress: a.content, pendingHidden: false };
    case "delta": {
      const base = { ...t, waiting: due(t), pendingHidden: true };
      if (t.live === null) {
        const next = push(base, { kind: "assistant", content: a.content, streaming: true });
        return { ...next, live: t.nextId };
      }
      return {
        ...base,
        messages: t.messages.map((m) => (m.id === t.live ? { ...m, content: m.content + a.content } : m)),
      };
    }
    case "message": {
      // The whole reply supersedes the streamed text (a stopped turn's
      // partial text becomes "Stopped.").
      const media = a.media?.length ? a.media : undefined;
      const next =
        t.live === null
          ? push(t, { kind: "assistant", content: a.content, media })
          : {
              ...t,
              messages: t.messages.map((m) =>
                m.id === t.live ? { ...m, content: a.content, streaming: false, media } : m,
              ),
            };
      return { ...next, live: null, waiting: Math.max(0, t.waiting - 1), progress: null, pendingHidden: false };
    }
    case "error":
      return push(t, { kind: "error", content: a.content });
    case "disconnected":
      // The reconnect's history frame replaces any partial text.
      return {
        ...t,
        messages: t.messages.map((m) => (m.streaming ? { ...m, streaming: false } : m)),
        waiting: 0,
        live: null,
        progress: null,
        pendingHidden: false,
      };
  }
}
