// Markdown options shared by the chat bubbles and the tests (plain .ts so
// `node --test` can load it). react-markdown never passes raw HTML
// through: without rehype-raw, HTML in the source stays text.
import { createElement, type ComponentProps } from "react";
import type { Components, Options } from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "./highlight.ts";
import { isMediaImage } from "./media.ts";

const allowedSchemes = new Set(["http:", "https:", "mailto:"]);

// safeUrl keeps http(s) and mailto URLs and scheme-less references
// (#anchor, /path, ./rel); every other scheme (javascript:, data:, …)
// becomes "".
export function safeUrl(url: string): string {
  const s = url.trim();
  // A scheme is letters before the first ":" that comes before any / ? #.
  const m = /^([a-z][a-z0-9+.-]*):/i.exec(s);
  if (!m) {
    // Browsers ignore tabs and newlines inside schemes ("java\tscript:").
    return /^[^/?#]*:/.test(s.replace(/[\t\n\r]/g, "")) ? "" : s;
  }
  return allowedSchemes.has(m[1].toLowerCase() + ":") ? s : "";
}

// Links open in a new tab; a blocked URL leaves only the text.
function Link({ href, children }: ComponentProps<"a">) {
  if (!href) return createElement("span", null, children);
  const local = href.startsWith("#");
  return createElement(
    "a",
    local ? { href } : { href, target: "_blank", rel: "noopener noreferrer" },
    children,
  );
}

// Workspace images the gateway serves (/ui/media/…) are shown, linked to
// the full-size file. Any other image is shown as a link: the CSP blocks
// remote images, and loading them would let a reply ping arbitrary hosts.
function Image({ src, alt }: ComponentProps<"img">) {
  const url = typeof src === "string" ? src : undefined;
  if (url && isMediaImage(url)) {
    return createElement(
      "a",
      { href: url, target: "_blank", rel: "noopener noreferrer", className: "media-link" },
      createElement("img", { src: url, alt: alt ?? "", loading: "lazy", className: "media-img" }),
    );
  }
  return Link({ href: url, children: alt || url || "image" });
}

export const components: Components = { a: Link, img: Image };

export const markdownOptions: Options = {
  remarkPlugins: [remarkGfm, remarkBreaks],
  rehypePlugins: [rehypeHighlight],
  urlTransform: safeUrl,
  components,
  skipHtml: true,
};
