import { useEffect, useLayoutEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";

interface Props {
  canSend: boolean; // connected and history loaded
  busy: boolean; // a reply is due, so Stop is shown
  onSend: (text: string) => boolean;
  onStop: () => void;
}

// Composer is the message box: Enter sends, Shift+Enter starts a new line.
export function Composer({ canSend, busy, onSend, onStop }: Props) {
  const [text, setText] = useState("");
  const input = useRef<HTMLTextAreaElement>(null);

  // Grow with the text, up to the CSS max-height. (Setting style through
  // the DOM is allowed by the CSP; style attributes in markup are not.)
  useLayoutEffect(() => {
    const el = input.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = el.scrollHeight + "px";
  }, [text]);

  useEffect(() => {
    input.current?.focus();
  }, []);

  const send = () => {
    const t = text.trim();
    if (!t || !canSend) return;
    if (onSend(t)) setText("");
  };

  const onSubmit = (ev: FormEvent) => {
    ev.preventDefault();
    send();
  };

  const onKeyDown = (ev: KeyboardEvent<HTMLTextAreaElement>) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.nativeEvent.isComposing) {
      ev.preventDefault();
      send();
    }
  };

  return (
    <form id="composer" autoComplete="off" onSubmit={onSubmit}>
      <textarea
        id="input"
        ref={input}
        rows={1}
        placeholder="Message nanobot (Enter to send, Shift+Enter for a new line)"
        aria-label="Message"
        value={text}
        onChange={(ev) => setText(ev.target.value)}
        onKeyDown={onKeyDown}
      />
      {busy && (
        <button id="stop" type="button" title="Stop the reply in progress" disabled={!canSend} onClick={onStop}>
          Stop
        </button>
      )}
      <button id="send" type="submit" disabled={!canSend}>
        Send
      </button>
    </form>
  );
}
