import { useState, type ClipboardEvent, type KeyboardEvent } from "react";
import { addTags } from "./settings";

// TagInput edits a list of short strings as chips: Enter, comma or a
// space adds what is typed (pasted lists are split the same way),
// Backspace in the empty box removes the last chip, and each chip's ×
// removes it. Whatever is typed is added on blur, so a Save click right
// after typing keeps it.
export function TagInput({
  id,
  value,
  onChange,
  placeholder,
  removeLabel,
  disabled,
}: {
  id?: string;
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  removeLabel: (tag: string) => string;
  disabled?: boolean;
}) {
  const [text, setText] = useState("");

  const commit = (raw: string) => {
    const next = addTags(value, raw);
    if (next !== value) onChange(next);
    setText("");
  };

  const onKeyDown = (ev: KeyboardEvent<HTMLInputElement>) => {
    if (ev.nativeEvent.isComposing) return;
    if (ev.key === "Enter" || ev.key === "," || ev.key === " " || ev.key === "Tab") {
      if (!text.trim()) {
        if (ev.key === "Enter") ev.preventDefault(); // never submit the form from here
        return;
      }
      ev.preventDefault();
      commit(text);
    } else if (ev.key === "Backspace" && text === "" && value.length) {
      onChange(value.slice(0, -1));
    }
  };

  const onPaste = (ev: ClipboardEvent<HTMLInputElement>) => {
    const pasted = ev.clipboardData.getData("text");
    if (!/[\s,]/.test(pasted.trim())) return;
    ev.preventDefault();
    commit(text + pasted);
  };

  return (
    <div className="tag-input" data-disabled={disabled || undefined}>
      {value.map((tag) => (
        <span key={tag} className="tool-chip">
          {tag}
          <button type="button" aria-label={removeLabel(tag)} title={removeLabel(tag)} disabled={disabled} onClick={() => onChange(value.filter((v) => v !== tag))}>
            ×
          </button>
        </span>
      ))}
      <input
        id={id}
        type="text"
        spellCheck={false}
        autoCapitalize="off"
        disabled={disabled}
        placeholder={placeholder}
        value={text}
        onChange={(ev) => setText(ev.target.value)}
        onKeyDown={onKeyDown}
        onPaste={onPaste}
        onBlur={() => text.trim() && commit(text)}
      />
    </div>
  );
}
