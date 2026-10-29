import { Icon } from "./Icon";
import { useT } from "./i18n";
import type { MediaRef } from "./media";

// Attachments shows a message's files under its bubble: images as
// thumbnails linked to the full-size file, other files as download chips,
// remote URLs as chips opening in a new tab.
export function Attachments({ media }: { media: MediaRef[] }) {
  const t = useT();
  if (media.length === 0) return null;
  return (
    <div className="attachments">
      {media.map((m) =>
        m.kind === "link" ? (
          <a key={m.url} className="file-chip" href={m.url} target="_blank" rel="noopener noreferrer" title={m.url}>
            <Icon name="link" /> {m.name}
          </a>
        ) : m.kind === "image" ? (
          <a key={m.url} className="media-link" href={m.url} target="_blank" rel="noopener noreferrer" title={m.name}>
            <img className="media-img" src={m.url} alt={m.name} loading="lazy" />
          </a>
        ) : (
          <a key={m.url} className="file-chip" href={m.url} download={m.name} title={t("messages.download", { name: m.name })}>
            <span className="file-ico" aria-hidden="true">{fileExt(m.name)}</span> {m.name}
          </a>
        ),
      )}
    </div>
  );
}

// fileExt is a file's extension for its icon, such as "PDF", or "FILE".
export function fileExt(name: string): string {
  const m = /\.([A-Za-z0-9]{1,4})$/.exec(name);
  return m ? m[1].toUpperCase() : "FILE";
}
