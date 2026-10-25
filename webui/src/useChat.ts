import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import { checkSignedIn } from "./auth";
import { mediaRefs, type MediaRef } from "./media";
import { chatFrame, parseFrame, socketURL, storeChatId, storedChatId, type ClientFrame } from "./protocol";
import { emptyTranscript, reduce } from "./transcript";

export type ConnState = "connecting" | "open" | "closed";

export interface Connection {
  state: ConnState;
  retryIn: number; // seconds until the next attempt, when closed
}

// useChat keeps one WebSocket to /ws for the current chat, reconnecting
// with backoff, and folds its frames into the transcript. onSaved runs
// after each history and message frame, when the chat list may have
// changed.
export function useChat(onSaved: () => void) {
  const [transcript, dispatch] = useReducer(reduce, emptyTranscript);
  const [conn, setConn] = useState<Connection>({ state: "connecting", retryIn: 0 });
  const [chatId, setChatId] = useState<string | null>(storedChatId);
  // Bumped to drop the socket and connect again (switching chats).
  const [generation, setGeneration] = useState(0);
  const ws = useRef<WebSocket | null>(null);
  const savedRef = useRef(onSaved);
  savedRef.current = onSaved;

  useEffect(() => {
    let disposed = false;
    let retryDelay = 1000;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;

    const connect = () => {
      setConn({ state: "connecting", retryIn: 0 });
      const sock = new WebSocket(socketURL(storedChatId()));
      ws.current = sock;
      sock.onopen = () => {
        retryDelay = 1000;
      };
      sock.onmessage = (ev) => {
        if (disposed) return;
        const frame = parseFrame(ev.data);
        if (!frame) return;
        switch (frame.type) {
          case "hello":
            storeChatId(frame.chat_id);
            setChatId(frame.chat_id);
            break;
          case "history":
            dispatch({ type: "history", messages: frame.messages ?? [] });
            // Sending is enabled only now, so nothing typed meanwhile is
            // wiped by the replaced list.
            setConn({ state: "open", retryIn: 0 });
            savedRef.current();
            break;
          case "progress":
            dispatch({ type: "progress", content: frame.content ?? "" });
            break;
          case "delta":
            dispatch({ type: "delta", content: frame.content ?? "" });
            break;
          case "message":
            dispatch({ type: "message", content: frame.content ?? "", media: mediaRefs(frame.media) });
            savedRef.current(); // the turn is saved before its reply is sent
            break;
          case "error":
            dispatch({ type: "error", content: frame.content ?? "" });
            break;
        }
      };
      sock.onclose = () => {
        if (disposed) return;
        ws.current = null;
        void checkSignedIn();
        dispatch({ type: "disconnected" });
        setConn({ state: "closed", retryIn: Math.round(retryDelay / 1000) });
        retryTimer = setTimeout(connect, retryDelay);
        retryDelay = Math.min(retryDelay * 2, 10000);
      };
    };

    connect();
    return () => {
      disposed = true;
      clearTimeout(retryTimer);
      const sock = ws.current;
      ws.current = null;
      sock?.close();
    };
  }, [generation]);

  const sendFrame = useCallback((frame: ClientFrame): boolean => {
    const sock = ws.current;
    if (!sock || sock.readyState !== WebSocket.OPEN) return false;
    sock.send(JSON.stringify(frame));
    return true;
  }, []);

  // send sends a chat message with its attached media, which the bubble
  // shows and the model receives (images as image parts); "/stop" stops
  // the running turn instead (only the stopped turn is answered). It
  // reports whether it was sent.
  const send = useCallback(
    (text: string, media?: MediaRef[]): boolean => {
      if (text.toLowerCase() === "/stop" && !media?.length) return sendFrame({ type: "stop" });
      if (!sendFrame(chatFrame(text, media))) return false;
      dispatch({ type: "sent", content: text, media });
      return true;
    },
    [sendFrame],
  );

  // stop stops the running turn, which is answered "Stopped.".
  const stop = useCallback(() => {
    sendFrame({ type: "stop" });
  }, [sendFrame]);

  // switchChat reconnects to chat id, or to a new chat when id is null;
  // the server's history frame then fills the page.
  const switchChat = useCallback(
    (id: string | null) => {
      if (id && id === storedChatId() && ws.current?.readyState === WebSocket.OPEN) return;
      storeChatId(id);
      setChatId(id);
      // Clearing now avoids showing the old chat while connecting.
      dispatch({ type: "history", messages: [] });
      setGeneration((g) => g + 1);
    },
    [],
  );

  return { transcript, conn, chatId, send, stop, switchChat };
}
