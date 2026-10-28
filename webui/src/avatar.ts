// Helpers for the chat list's letter avatars: a stable color per chat id
// and the first character of its title.

/** AVATAR_COLORS are backgrounds that read well under white text in both
 * light and dark themes. */
export const AVATAR_COLORS = [
  "#2f6fed",
  "#0b7285",
  "#2b7a3d",
  "#9a6700",
  "#c2410c",
  "#c0392b",
  "#a626a4",
  "#6d28d9",
] as const;

/** avatarColor picks a palette color from a hash (FNV-1a) of chatId, so a
 * chat keeps its color across reloads and renames. */
export function avatarColor(chatId: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < chatId.length; i++) {
    h ^= chatId.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return AVATAR_COLORS[(h >>> 0) % AVATAR_COLORS.length];
}

/** avatarLetter is the first grapheme of title, upper-cased, or "?" when
 * the title is blank. */
export function avatarLetter(title: string | undefined): string {
  const s = (title ?? "").trim();
  if (!s) return "?";
  let first = Array.from(s)[0];
  if (typeof Intl !== "undefined" && "Segmenter" in Intl) {
    const seg = new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(s);
    first = seg[Symbol.iterator]().next().value?.segment ?? first;
  }
  return first.toLocaleUpperCase();
}
