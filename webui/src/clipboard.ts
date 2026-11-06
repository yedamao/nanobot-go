const imageExtensions: Record<string, string> = {
  "svg+xml": "svg",
  "x-icon": "ico",
  "vnd.microsoft.icon": "ico",
};

function timestamp(date: Date): string {
  const part = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}${part(date.getMonth() + 1)}${part(date.getDate())}-${part(date.getHours())}${part(date.getMinutes())}${part(date.getSeconds())}`;
}

export function nameClipboardImage(file: File, now = new Date()): File {
  if (file.name) return file;
  const subtype = file.type.slice("image/".length).toLowerCase();
  const extension = imageExtensions[subtype] ?? (subtype.replace(/[^a-z0-9]/g, "") || "png");
  return new File([file], `paste-${timestamp(now)}.${extension}`, { type: file.type, lastModified: file.lastModified });
}

export function clipboardImages(data: DataTransfer, now = new Date()): File[] {
  const itemImages = Array.from(data.items)
    .filter((item) => item.kind === "file" && item.type.startsWith("image/"))
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null);
  const images = itemImages.length > 0 ? itemImages : Array.from(data.files).filter((file) => file.type.startsWith("image/"));
  return images.map((file) => nameClipboardImage(file, now));
}
