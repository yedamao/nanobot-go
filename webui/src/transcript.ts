import type { MediaRef } from "./media";
import type { HistoryMessage } from "./protocol";

export interface Message {
  id: number;
  // "thinking" is the model's streamed reasoning for a turn: shown apart
  // from the reply and never stored, so history does not bring it back.
  kind: "user" | "assistant" | "thinking" | "error";
  content: string;
  streaming?: boolean; // the live bubble deltas (or thinking) append to
  media?: MediaRef[]; // files shown under the bubble
}

// Transcript is what the page shows of the current chat.
export interface Transcript {
  messages: Message[];
  waiting: number; // messages sent and not answered yet
  live: number | null; // id of the streaming bubble, until its message frame
  thinking: number | null; // id of the turn's thinking, until its message frame
  progress: string | null; // the running turn's status, such as its tool
  pendingHidden: boolean; // the pending bubble hides while text or thinking streams
  nextId: number;
}

export type Action =
  | { type: "history"; messages: HistoryMessage[] }
  | { type: "sent"; content: string; media?: MediaRef[] }
  | { type: "progress"; content: string }
  | { type: "delta"; content: string }
  | { type: "thinking"; content: string }
  | { type: "thinking_end" }
  | { type: "message"; content: string; media?: MediaRef[] }
  | { type: "error"; content: string }
  | { type: "disconnected" };

export const emptyTranscript: Transcript = {
  messages: [],
  waiting: 0,
  live: null,
  thinking: null,
  progress: null,
  pendingHidden: false,
  nextId: 1,
};

// settle stops every streaming bubble, the thinking included.
function settle(messages: Message[]): Message[] {
  return messages.map((m) => (m.streaming ? { ...m, streaming: false } : m));
}

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
    case "thinking": {
      // A turn's thinking, across its model requests, goes into one panel;
      // the server separates later runs of it with a blank line.
      const base = { ...t, waiting: due(t), pendingHidden: true };
      if (t.thinking === null) {
        const next = push(base, { kind: "thinking", content: a.content.trimStart(), streaming: true });
        return { ...next, thinking: t.nextId };
      }
      return {
        ...base,
        messages: t.messages.map((m) =>
          m.id === t.thinking ? { ...m, content: m.content + a.content, streaming: true } : m,
        ),
      };
    }
    case "thinking_end":
      // The panel collapses; the pending bubble shows again until the
      // answer streams or a tool runs.
      return {
        ...t,
        messages: t.messages.map((m) => (m.id === t.thinking ? { ...m, streaming: false } : m)),
        pendingHidden: false,
      };
    case "message": {
      // The whole reply supersedes the streamed text (a stopped turn's
      // partial text becomes "Stopped."). The turn's thinking stays above
      // it, collapsed, until the next history frame.
      const media = a.media?.length ? a.media : undefined;
      const settled = { ...t, messages: settle(t.messages) };
      const next =
        t.live === null
          ? push(settled, { kind: "assistant", content: a.content, media })
          : {
              ...settled,
              messages: settled.messages.map((m) => (m.id === t.live ? { ...m, content: a.content, media } : m)),
            };
      return {
        ...next,
        live: null,
        thinking: null,
        waiting: Math.max(0, t.waiting - 1),
        progress: null,
        pendingHidden: false,
      };
    }
    case "error":
      return push(t, { kind: "error", content: a.content });
    case "disconnected":
      // The reconnect's history frame replaces any partial text.
      return {
        ...t,
        messages: settle(t.messages),
        waiting: 0,
        live: null,
        thinking: null,
        progress: null,
        pendingHidden: false,
      };
  }
}
