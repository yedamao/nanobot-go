import { useEffect, useRef } from "react";
import { createPortal } from "react-dom";
import { Icon } from "./Icon";
import { useT } from "./i18n";
import { wrapIndex } from "./lightbox";
import type { MediaRef } from "./media";

export function ImageLightbox({
  images,
  index,
  onIndexChange,
  onClose,
}: {
  images: MediaRef[];
  index: number;
  onIndexChange: (index: number) => void;
  onClose: () => void;
}) {
  const t = useT();
  const closeRef = useRef<HTMLButtonElement>(null);
  const image = images[index];
  const multiple = images.length > 1;

  useEffect(() => {
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    closeRef.current?.focus();
    return () => {
      document.body.style.overflow = overflow;
    };
  }, []);

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
      if (multiple && event.key === "ArrowLeft") onIndexChange(wrapIndex(index - 1, images.length));
      if (multiple && event.key === "ArrowRight") onIndexChange(wrapIndex(index + 1, images.length));
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [images.length, index, multiple, onClose, onIndexChange]);

  return createPortal(
    <div className="lightbox" role="dialog" aria-modal="true" aria-label={t("messages.imagePreview")} onMouseDown={(event) => {
      if (event.target === event.currentTarget) onClose();
    }}>
      <button ref={closeRef} type="button" className="lightbox-btn lightbox-close" aria-label={t("messages.closePreview")} onClick={onClose}>
        <Icon name="x" />
      </button>
      {multiple && (
        <button type="button" className="lightbox-btn lightbox-prev" aria-label={t("messages.previousImage")} onClick={() => onIndexChange(wrapIndex(index - 1, images.length))}>
          <Icon name="arrow-left" />
        </button>
      )}
      <img className="lightbox-image" src={image.url} alt={image.name} />
      {multiple && (
        <button type="button" className="lightbox-btn lightbox-next" aria-label={t("messages.nextImage")} onClick={() => onIndexChange(wrapIndex(index + 1, images.length))}>
          <Icon name="arrow-left" />
        </button>
      )}
    </div>,
    document.body,
  );
}
