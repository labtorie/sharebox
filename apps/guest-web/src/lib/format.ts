export function formatBytes(n: number): string {
  if (n < 0 || !isFinite(n)) return "—";
  if (n < 1023.5) return `${Math.round(n)} B`;
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function formatSpeed(bps: number): string {
  return bps >= 0.5 ? `${formatBytes(bps)}/s` : "";
}

export function formatDate(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 1980) return "—";
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString(undefined, { year: d.getFullYear() === now.getFullYear() ? undefined : "numeric", month: "short", day: "numeric" });
}

export function formatETA(remaining: number, speed: number): string {
  if (speed <= 0 || remaining <= 0) return "";
  const s = Math.round(remaining / speed);
  if (s < 60) return `${s}s left`;
  if (s < 3600) return `${Math.round(s / 60)} min left`;
  return `${(s / 3600).toFixed(1)} h left`;
}

export type FileType = "folder" | "image" | "video" | "audio" | "archive" | "pdf" | "code" | "text" | "sheet" | "doc" | "slides" | "file";

const ext: Record<string, FileType> = {};
const add = (t: FileType, list: string) => list.split(" ").forEach((e) => (ext[e] = t));
add("image", "jpg jpeg png gif webp heic heif bmp tiff tif svg raw cr2 nef arw dng avif ico psd");
add("video", "mp4 mov mkv avi webm m4v wmv flv mpg mpeg 3gp");
add("audio", "mp3 wav flac aac m4a ogg opus wma aiff alac");
add("archive", "zip rar 7z tar gz tgz bz2 xz zst dmg iso pkg");
add("pdf", "pdf");
add("code", "js ts tsx jsx go py rb rs java c h cpp cs swift kt php html css scss json yaml yml toml xml sh sql");
add("text", "txt md rtf log csv ini cfg");
add("sheet", "xls xlsx numbers ods tsv");
add("doc", "doc docx pages odt");
add("slides", "ppt pptx key odp");

export function fileType(name: string, isDir: boolean): FileType {
  if (isDir) return "folder";
  const i = name.lastIndexOf(".");
  return (i > 0 && ext[name.slice(i + 1).toLowerCase()]) || "file";
}

const labels: Record<FileType, string> = {
  folder: "Folder", image: "Image", video: "Video", audio: "Audio", archive: "Archive", pdf: "PDF",
  code: "Code", text: "Text", sheet: "Spreadsheet", doc: "Document", slides: "Presentation", file: "File",
};

export function typeLabel(name: string, isDir: boolean): string {
  const t = fileType(name, isDir);
  if (t === "file") {
    const i = name.lastIndexOf(".");
    return i > 0 ? name.slice(i + 1).toUpperCase() + " file" : "File";
  }
  return labels[t];
}
