import { isValidElement, memo, useRef, useState, type ComponentProps } from "react";
import ReactMarkdown from "react-markdown";
import { components, markdownOptions } from "./markdown-options";
import { useT } from "./i18n";

// codeLanguage is a fenced block's language, from its <code> class.
function codeLanguage(children: unknown): string {
  if (!isValidElement<{ className?: string }>(children)) return "";
  return /language-(\S+)/.exec(children.props.className ?? "")?.[1] ?? "";
}

// CodeBlock frames a fenced block under a header with its language and a
// copy button.
function CodeBlock({ children }: ComponentProps<"pre">) {
  const t = useT();
  const ref = useRef<HTMLPreElement>(null);
  const [copied, setCopied] = useState(false);
  const copy = () => {
    const text = ref.current?.textContent ?? "";
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <div className="code-block">
      <header>
        <span>{codeLanguage(children)}</span>
        <button type="button" className="copy" onClick={copy}>
          {t(copied ? "messages.copied" : "messages.copy")}
        </button>
      </header>
      <pre ref={ref}>{children}</pre>
    </div>
  );
}

const options = { ...markdownOptions, components: { ...components, pre: CodeBlock } };

// Markdown renders a reply. A streaming reply re-renders as deltas grow;
// an unclosed fence is a code block up to the end of the text (CommonMark),
// so a half-streamed fence already shows as code. Memoized so finished
// bubbles are not re-parsed when a new delta arrives.
export const Markdown = memo(function Markdown({ text }: { text: string }) {
  return (
    <div className="md">
      <ReactMarkdown {...options}>{text}</ReactMarkdown>
    </div>
  );
});
