// The gateway's /ui/ HTTP endpoints, fetched relative to the page (/ui/).

// GET /ui/chats lists the stored chats; PATCH /ui/chats/{chat_id}
// {"title":"..."} renames one (answered with its entry) and DELETE
// /ui/chats/{chat_id} deletes it (204).
export interface ChatEntry {
  chat_id: string;
  updated_at?: string;
  title?: string;
  preview?: string;
}

// requestError is the reason a response failed: its text, or the status.
async function requestError(resp: Response): Promise<Error> {
  return new Error((await resp.text()).trim() || "HTTP " + resp.status);
}

export async function listChats(): Promise<ChatEntry[]> {
  const resp = await fetch("chats", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { chats?: ChatEntry[] }).chats ?? [];
}

async function chatRequest(id: string, method: string, body?: unknown): Promise<void> {
  const resp = await fetch("chats/" + encodeURIComponent(id), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) throw await requestError(resp);
}

export function renameChat(id: string, title: string): Promise<void> {
  return chatRequest(id, "PATCH", { title });
}

export function deleteChat(id: string): Promise<void> {
  return chatRequest(id, "DELETE");
}
