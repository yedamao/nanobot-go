import { memo, useRef, useState, type ComponentProps } from "react";
import ReactMarkdown from "react-markdown";
import { components, markdownOptions } from "./markdown-options";

// CodeBlock frames a fenced block with a copy button.
function CodeBlock({ children }: ComponentProps<"pre">) {
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
      <button type="button" className="copy" onClick={copy}>
        {copied ? "Copied" : "Copy"}
      </button>
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
