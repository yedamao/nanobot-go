import { useRef, useState } from "react";
import { Icon } from "./Icon";
import { ImageLightbox } from "./ImageLightbox";
import { useT } from "./i18n";
import type { MediaRef } from "./media";

// Attachments shows a message's files under its bubble: images as
// lightbox thumbnails, other files as download chips,
// remote URLs as chips opening in a new tab.
export function Attachments({ media }: { media: MediaRef[] }) {
  const t = useT();
  const [openIndex, setOpenIndex] = useState<number | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const images = media.filter((m) => m.kind === "image");

  function closeLightbox() {
    setOpenIndex(null);
    requestAnimationFrame(() => triggerRef.current?.focus());
  }

  if (media.length === 0) return null;
  return (
    <>
      <div className="attachments">
        {media.map((m) =>
          m.kind === "link" ? (
            <a key={m.url} className="file-chip" href={m.url} target="_blank" rel="noopener noreferrer" title={m.url}>
              <Icon name="link" /> {m.name}
            </a>
          ) : m.kind === "image" ? (
            <button key={m.url} type="button" className="media-link" title={m.name} aria-label={t("messages.previewImage", { name: m.name })} onClick={(event) => {
              triggerRef.current = event.currentTarget;
              setOpenIndex(images.indexOf(m));
            }}>
              <img className="media-img" src={m.url} alt={m.name} loading="lazy" />
            </button>
          ) : (
            <a key={m.url} className="file-chip" href={m.url} download={m.name} title={t("messages.download", { name: m.name })}>
              <span className="file-ico" aria-hidden="true">{fileExt(m.name)}</span> {m.name}
            </a>
          ),
        )}
      </div>
      {openIndex !== null && <ImageLightbox images={images} index={openIndex} onIndexChange={setOpenIndex} onClose={closeLightbox} />}
    </>
  );
}

// fileExt is a file's extension for its icon, such as "PDF", or "FILE".
export function fileExt(name: string): string {
  const m = /\.([A-Za-z0-9]{1,4})$/.exec(name);
  return m ? m[1].toUpperCase() : "FILE";
}
