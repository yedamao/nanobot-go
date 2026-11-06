const mimeExtensions: Record<string, string> = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/gif": "gif",
  "image/webp": "webp",
  "image/svg+xml": "svg",
  "image/bmp": "bmp",
  "image/tiff": "tiff",
  "image/x-icon": "ico",
  "image/vnd.microsoft.icon": "ico",
  "image/avif": "avif",
  "application/pdf": "pdf",
  "application/zip": "zip",
  "application/x-zip-compressed": "zip",
  "application/gzip": "gz",
  "application/x-gzip": "gz",
  "application/x-tar": "tar",
  "application/json": "json",
  "application/ld+json": "jsonld",
  "application/xml": "xml",
  "application/javascript": "js",
  "application/x-javascript": "js",
  "application/wasm": "wasm",
  "text/plain": "txt",
  "text/csv": "csv",
  "text/html": "html",
  "text/css": "css",
  "text/xml": "xml",
  "text/markdown": "md",
  "text/javascript": "js",
  "text/typescript": "ts",
  "text/jsx": "jsx",
  "text/tsx": "tsx",
  "text/x-c": "c",
  "text/x-c++": "cpp",
  "text/x-csharp": "cs",
  "text/x-go": "go",
  "text/x-java-source": "java",
  "text/x-python": "py",
  "text/x-ruby": "rb",
  "text/x-rust": "rs",
  "text/x-shellscript": "sh",
};

function timestamp(date: Date): string {
  const part = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}${part(date.getMonth() + 1)}${part(date.getDate())}-${part(date.getHours())}${part(date.getMinutes())}${part(date.getSeconds())}`;
}

export function nameClipboardFile(file: File, now = new Date()): File {
  if (file.name) return file;
  const extension = mimeExtensions[file.type.toLowerCase()] ?? "bin";
  return new File([file], `paste-${timestamp(now)}.${extension}`, { type: file.type, lastModified: file.lastModified });
}

export function clipboardFiles(data: DataTransfer, now = new Date()): File[] {
  const itemFiles = Array.from(data.items)
    .filter((item) => item.kind === "file")
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null);
  const files = itemFiles.length > 0 ? itemFiles : Array.from(data.files);
  return files.map((file) => nameClipboardFile(file, now));
}

// Compatibility aliases for callers that still specifically handle images.
export const nameClipboardImage = nameClipboardFile;

export function clipboardImages(data: DataTransfer, now = new Date()): File[] {
  return clipboardFiles(data, now).filter((file) => file.type.startsWith("image/"));
}
