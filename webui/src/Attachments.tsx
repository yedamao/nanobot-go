import type { MediaRef } from "./media";

// Attachments shows a message's files under its bubble: images as
// thumbnails linked to the full-size file, other files as download chips.
export function Attachments({ media }: { media: MediaRef[] }) {
  if (media.length === 0) return null;
  return (
    <div className="attachments">
      {media.map((m) =>
        m.kind === "image" ? (
          <a key={m.url} className="media-link" href={m.url} target="_blank" rel="noopener noreferrer" title={m.name}>
            <img className="media-img" src={m.url} alt={m.name} loading="lazy" />
          </a>
        ) : (
          <a key={m.url} className="file-chip" href={m.url} download={m.name} title={"Download " + m.name}>
            <span aria-hidden="true">📎</span> {m.name}
          </a>
        ),
      )}
    </div>
  );
}
