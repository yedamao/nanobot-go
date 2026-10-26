// Files the gateway serves from the agent's workspace under /ui/media/
// (same origin, behind the same sign-in as the rest of the page). Plain
// .ts so `node --test` can load it.

export const MEDIA_PREFIX = "/ui/media/";

// MediaRef is a file attached to a message: the media list of a message
// frame, or a markdown image in a user message. A "link" is a remote
// http(s) URL the agent sent; it is shown as a link, since the CSP keeps
// the page from loading remote images.
export interface MediaRef {
  url: string;
  name: string;
  kind: "image" | "file" | "link";
}

const imageExt = /\.(png|jpe?g|gif|webp|svg)$/i;

// isMediaURL reports whether url is a plain same-origin media URL: under
// MEDIA_PREFIX with no dot segments, backslashes, query or fragment.
export function isMediaURL(url: string): boolean {
  if (!url.startsWith(MEDIA_PREFIX) || /[\\?#]/.test(url)) return false;
  let path: string;
  try {
    path = decodeURIComponent(url.slice(MEDIA_PREFIX.length));
  } catch {
    return false;
  }
  return path !== "" && !path.split("/").some((s) => s === "" || s.startsWith("."));
}

// isMediaImage reports whether url is a media URL the page previews.
export function isMediaImage(url: string): boolean {
  return isMediaURL(url) && imageExt.test(url);
}

function mediaName(url: string): string {
  const last = url.slice(url.lastIndexOf("/") + 1);
  try {
    return decodeURIComponent(last);
  } catch {
    return last;
  }
}

// remoteURL returns url normalized when it is an absolute http(s) URL
// with a host, else null.
export function remoteURL(url: string): string | null {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  return (u.protocol === "http:" || u.protocol === "https:") && u.host !== "" ? u.href : null;
}

function remoteName(href: string): string {
  const u = new URL(href);
  return mediaName(u.pathname) || u.host;
}

// mediaRefs keeps the well-formed media URLs of a message frame's media
// list, and http(s) URLs as links; anything else (a malformed entry,
// another scheme) is dropped.
export function mediaRefs(v: unknown): MediaRef[] {
  if (!Array.isArray(v)) return [];
  const out: MediaRef[] = [];
  for (const m of v) {
    if (!m || typeof m !== "object") continue;
    const { url, name } = m as { url?: unknown; name?: unknown };
    if (typeof url !== "string") continue;
    if (!isMediaURL(url)) {
      const href = remoteURL(url);
      if (href) {
        out.push({ url: href, name: typeof name === "string" && name ? name : remoteName(href), kind: "link" });
      }
      continue;
    }
    out.push({
      url,
      name: typeof name === "string" && name ? name : mediaName(url),
      kind: imageExt.test(url) ? "image" : "file",
    });
  }
  return out;
}

// attachmentText is how an uploaded file is named in a chat message: a
// markdown image or link to its workspace path, which the agent can open
// with its file tools and the gateway turns into a preview.
export function attachmentText(a: { path: string; name: string; kind: string }): string {
  const label = a.name.replace(/[[\]\\]/g, "_");
  return (a.kind === "image" ? "!" : "") + "[" + label + "](" + a.path + ")";
}

const markdownImage =/!\[([^\]\n]*)\]\((\/ui\/media\/[^)\s]+)(?:\s+(?:"[^"\n]*"|'[^'\n]*'))?\)/g;

// imagesInText returns the media images a user message's markdown names
// (the gateway rewrites workspace paths to media URLs), so the plain-text
// bubble can show them.
export function imagesInText(text: string): MediaRef[] {
  const out: MediaRef[] = [];
  for (const m of text.matchAll(markdownImage)) {
    const url = m[2];
    if (isMediaImage(url) && !out.some((r) => r.url === url)) {
      out.push({ url, name: m[1] || mediaName(url), kind: "image" });
    }
  }
  return out;
}
