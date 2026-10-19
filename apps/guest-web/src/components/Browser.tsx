import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowDownToLine, ChevronRight, FolderPlus, FolderUp, Home, Loader2, RefreshCw, Search, Trash2, Upload, X, ArrowUpDown,
  ArrowUp, ArrowDown, FolderArchive, Lock,
} from "lucide-react";
import { Api, ApiError, type Entry, type SessionInfo } from "../lib/api";
import type { Transfers } from "../lib/transfers";
import { formatBytes, formatDate, typeLabel } from "../lib/format";
import { Button, Dialog, FileIcon, TextInput, cx } from "./ui";

type SortKey = "name" | "size" | "type" | "modTime";

interface Props {
  api: Api;
  session: SessionInfo;
  transfers: Transfers;
  toast: (kind: "error" | "info" | "success", text: string) => void;
}

interface UploadItem {
  file: File;
  rel: string; // relative path inside the dropped folder, "" for plain files
}

export function Browser({ api, session, transfers, toast }: Props) {
  const [path, setPath] = useState(() => decodeURIComponent(location.hash.startsWith("#/") ? location.hash.slice(2) : ""));
  const [entries, setEntries] = useState<Entry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [sort, setSort] = useState<{ key: SortKey; asc: boolean }>({ key: "name", asc: true });
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<Entry[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [mkdirOpen, setMkdirOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [toDelete, setToDelete] = useState<Entry | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const dirInput = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);
  const perms = session.permissions;

  const load = useCallback(
    async (p: string, quiet = false) => {
      if (!quiet) setLoading(true);
      setError(null);
      try {
        const r = await api.list(p);
        setEntries(r.entries);
        if (r.truncated) toast("info", "This folder is very large; only the first 50,000 items are shown.");
      } catch (e) {
        setError(e instanceof ApiError && e.status === 404 ? "This folder no longer exists." : (e as Error).message);
        setEntries([]);
      } finally {
        setLoading(false);
      }
    },
    [api, toast],
  );

  useEffect(() => {
    load(path);
    history.replaceState(null, "", location.pathname + location.search + (path ? "#/" + encodeURIComponent(path) : ""));
  }, [path, load]);

  useEffect(() => {
    transfers.onUploaded = (dir) => {
      if (dir === path) load(path, true);
    };
  }, [transfers, path, load]);

  // Heartbeat keeps the guest visible to the host and the token fresh.
  useEffect(() => {
    const t = setInterval(() => api.refreshSession().catch(() => {}), 30_000);
    return () => clearInterval(t);
  }, [api]);

  // Debounced search.
  useEffect(() => {
    const q = query.trim();
    if (!q) {
      setResults(null);
      return;
    }
    setSearching(true);
    const t = setTimeout(async () => {
      try {
        const r = await api.search("", q);
        setResults(r.results);
      } catch (e) {
        toast("error", (e as Error).message);
      } finally {
        setSearching(false);
      }
    }, 300);
    return () => clearTimeout(t);
  }, [query, api, toast]);

  const shown = useMemo(() => {
    const list = [...(results ?? entries ?? [])];
    const dir = sort.asc ? 1 : -1;
    list.sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
      let c = 0;
      switch (sort.key) {
        case "size":
          c = a.size - b.size;
          break;
        case "modTime":
          c = Date.parse(a.modTime) - Date.parse(b.modTime);
          break;
        case "type":
          c = typeLabel(a.name, a.isDir).localeCompare(typeLabel(b.name, b.isDir));
          break;
      }
      return (c || a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" })) * dir;
    });
    return list;
  }, [entries, results, sort]);

  const crumbs = path ? path.split("/") : [];

  const open = (e: Entry) => {
    if (e.isDir) {
      setQuery("");
      setPath(e.path);
    } else if (perms.download) transfers.download(e).catch((err) => toast("error", err.message));
  };

  const startUploads = async (items: UploadItem[]) => {
    if (!perms.upload) {
      toast("error", "The host does not allow uploads.");
      return;
    }
    const base = path;
    const dirs = new Set<string>();
    for (const it of items) {
      const parts = it.rel.split("/").slice(0, -1);
      for (let i = 1; i <= parts.length; i++) dirs.add(parts.slice(0, i).join("/"));
    }
    if (dirs.size && !perms.mkdir) {
      toast("error", "The host does not allow creating folders, so folders can't be uploaded. Upload the files instead.");
      return;
    }
    for (const d of [...dirs].sort((a, b) => a.split("/").length - b.split("/").length)) {
      try {
        await api.mkdir(base ? `${base}/${d}` : d);
      } catch (e) {
        if (!(e instanceof ApiError && e.status === 409)) {
          toast("error", `Couldn't create folder “${d}”: ${(e as Error).message}`);
          return;
        }
      }
    }
    if (dirs.size) load(path, true);
    for (const it of items) {
      const sub = it.rel.split("/").slice(0, -1).join("/");
      const target = [base, sub].filter(Boolean).join("/");
      if (session.maxUploadBytes > 0 && it.file.size > session.maxUploadBytes) {
        toast("error", `“${it.file.name}” is larger than the host allows (${formatBytes(session.maxUploadBytes)}).`);
        continue;
      }
      transfers.upload(it.file, target);
    }
  };

  const onDrop = async (ev: React.DragEvent) => {
    ev.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    const items = await collectDropped(ev.dataTransfer);
    if (items.length) startUploads(items);
  };

  const createFolder = async () => {
    const name = newName.trim();
    if (!name) return;
    try {
      await api.mkdir(path ? `${path}/${name}` : name);
      setMkdirOpen(false);
      setNewName("");
      load(path, true);
    } catch (e) {
      toast("error", e instanceof ApiError && e.status === 409 ? "A file or folder with that name already exists." : (e as Error).message);
    }
  };

  const doDelete = async () => {
    const e = toDelete;
    if (!e) return;
    setToDelete(null);
    try {
      await api.remove(e.path);
      toast("success", `Deleted “${e.name}”.`);
      if (results) setResults(results.filter((r) => r.path !== e.path));
      load(path, true);
    } catch (err) {
      toast("error", (err as Error).message);
    }
  };

  const header = (key: SortKey, label: string, cls = "") => (
    <th className={cx("px-3 py-2 font-medium", cls)}>
      <button
        className="inline-flex items-center gap-1 hover:text-zinc-900 dark:hover:text-zinc-100"
        onClick={() => setSort((s) => ({ key, asc: s.key === key ? !s.asc : true }))}
      >
        {label}
        {sort.key === key ? sort.asc ? <ArrowUp className="h-3 w-3" /> : <ArrowDown className="h-3 w-3" /> : <ArrowUpDown className="h-3 w-3 opacity-30" />}
      </button>
    </th>
  );

  return (
    <div
      className="relative flex min-h-0 flex-1 flex-col"
      onDragEnter={(e) => {
        if (!e.dataTransfer.types.includes("Files")) return;
        dragDepth.current++;
        setDragging(true);
      }}
      onDragLeave={() => {
        if (--dragDepth.current <= 0) setDragging(false);
      }}
      onDragOver={(e) => e.preventDefault()}
      onDrop={onDrop}
    >
      {/* toolbar */}
      <div className="flex flex-col gap-3 border-b border-zinc-200/70 px-4 py-3 sm:px-6 dark:border-zinc-800/70">
        <div className="flex flex-wrap items-center gap-2">
          <nav className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto text-sm" aria-label="Breadcrumb">
            <button onClick={() => setPath("")} className={cx("flex shrink-0 items-center gap-1.5 rounded-lg px-2 py-1 font-medium hover:bg-zinc-100 dark:hover:bg-zinc-800", !path && "text-zinc-900 dark:text-white")}>
              <Home className="h-4 w-4" />
              <span className="max-w-[10rem] truncate">{session.name}</span>
            </button>
            {crumbs.map((c, i) => (
              <span key={i} className="flex shrink-0 items-center gap-1">
                <ChevronRight className="h-3.5 w-3.5 text-zinc-400" />
                <button onClick={() => setPath(crumbs.slice(0, i + 1).join("/"))} className="max-w-[12rem] truncate rounded-lg px-2 py-1 hover:bg-zinc-100 dark:hover:bg-zinc-800">
                  {c}
                </button>
              </span>
            ))}
          </nav>
          <div className="relative w-full sm:w-64">
            <Search className="pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2 text-zinc-400" />
            <TextInput value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search this share" className="pl-9 pr-8" aria-label="Search" />
            {query && (
              <button onClick={() => setQuery("")} className="absolute top-1/2 right-2 -translate-y-1/2 rounded p-1 text-zinc-400 hover:text-zinc-700" aria-label="Clear search">
                {searching ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <X className="h-3.5 w-3.5" />}
              </button>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {perms.upload && (
            <>
              <Button variant="primary" onClick={() => fileInput.current?.click()}>
                <Upload className="h-4 w-4" /> Upload
              </Button>
              {perms.mkdir && (
                <Button onClick={() => dirInput.current?.click()} className="hidden sm:inline-flex">
                  <FolderUp className="h-4 w-4" /> Upload folder
                </Button>
              )}
            </>
          )}
          {perms.mkdir && (
            <Button onClick={() => setMkdirOpen(true)}>
              <FolderPlus className="h-4 w-4" /> <span className="hidden sm:inline">New folder</span>
            </Button>
          )}
          {perms.download && (
            <Button onClick={() => transfers.zip(path, crumbs[crumbs.length - 1] || session.name).catch((e) => toast("error", e.message))} title="Download this folder as a ZIP archive">
              <FolderArchive className="h-4 w-4" /> <span className="hidden sm:inline">Download all</span>
            </Button>
          )}
          <div className="flex-1" />
          {!perms.download && !perms.upload && (
            <span className="flex items-center gap-1 text-xs text-zinc-500">
              <Lock className="h-3.5 w-3.5" /> View only
            </span>
          )}
          <Button variant="ghost" onClick={() => load(path)} aria-label="Refresh">
            <RefreshCw className={cx("h-4 w-4", loading && "animate-spin")} />
          </Button>
        </div>
        <input ref={fileInput} type="file" multiple hidden onChange={(e) => { startUploads(Array.from(e.target.files ?? [], (file) => ({ file, rel: "" }))); e.target.value = ""; }} />
        <input
          ref={dirInput}
          type="file"
          hidden
          {...({ webkitdirectory: "", directory: "" } as Record<string, string>)}
          onChange={(e) => { startUploads(Array.from(e.target.files ?? [], (file) => ({ file, rel: file.webkitRelativePath || file.name }))); e.target.value = ""; }}
        />
      </div>

      {/* listing */}
      <div className="min-h-0 flex-1 overflow-y-auto pb-28">
        {results && (
          <div className="px-4 pt-3 text-xs text-zinc-500 sm:px-6">
            {results.length} result{results.length === 1 ? "" : "s"} for “{query.trim()}” across the whole share
          </div>
        )}
        {error ? (
          <Empty title="Couldn't open this folder" text={error} />
        ) : entries === null ? (
          <div className="flex justify-center py-20 text-zinc-400">
            <Loader2 className="h-6 w-6 animate-spin" />
          </div>
        ) : shown.length === 0 ? (
          results ? (
            <Empty title="No matches" text="Try a different search." />
          ) : (
            <Empty title="This folder is empty" text={perms.upload ? "Drop files here or use Upload to add some." : "There's nothing here yet."} />
          )
        ) : (
          <table className="w-full table-fixed text-sm">
            <thead className="sticky top-0 z-10 bg-zinc-50/90 text-left text-xs text-zinc-500 backdrop-blur dark:bg-zinc-950/90">
              <tr>
                {header("name", "Name", "pl-4 sm:pl-6")}
                {header("size", "Size", "hidden w-28 sm:table-cell")}
                {header("type", "Kind", "hidden w-36 md:table-cell")}
                {header("modTime", "Modified", "hidden w-36 sm:table-cell")}
                <th className="w-24 pr-4 sm:pr-6" />
              </tr>
            </thead>
            <tbody>
              {shown.map((e) => (
                <tr key={e.path} className="group border-t border-zinc-100 transition-colors hover:bg-white dark:border-zinc-900 dark:hover:bg-zinc-900/60" onDoubleClick={() => open(e)}>
                  <td className="py-2 pl-4 sm:pl-6">
                    <button onClick={() => open(e)} className="flex w-full min-w-0 items-center gap-3 text-left" disabled={!e.isDir && !perms.download}>
                      <FileIcon name={e.name} isDir={e.isDir} />
                      <span className="min-w-0">
                        <span className="block truncate font-medium" title={e.name}>
                          {e.name}
                        </span>
                        <span className="block truncate text-xs text-zinc-500 sm:hidden">
                          {e.isDir ? "Folder" : formatBytes(e.size)} · {formatDate(e.modTime)}
                        </span>
                        {results && e.path.includes("/") && <span className="block truncate text-xs text-zinc-400">in {e.path.split("/").slice(0, -1).join(" / ")}</span>}
                      </span>
                    </button>
                  </td>
                  <td className="hidden px-3 text-zinc-500 tabular-nums sm:table-cell">{e.isDir ? "—" : formatBytes(e.size)}</td>
                  <td className="hidden truncate px-3 text-zinc-500 md:table-cell">{typeLabel(e.name, e.isDir)}</td>
                  <td className="hidden px-3 text-zinc-500 sm:table-cell">{formatDate(e.modTime)}</td>
                  <td className="pr-4 sm:pr-6">
                    <div className="flex justify-end gap-0.5 opacity-100 transition sm:opacity-0 sm:group-hover:opacity-100 sm:focus-within:opacity-100">
                      {perms.download && (
                        <IconBtn
                          label={e.isDir ? "Download as ZIP" : "Download"}
                          onClick={() => (e.isDir ? transfers.zip(e.path, e.name) : transfers.download(e)).catch((err) => toast("error", err.message))}
                        >
                          <ArrowDownToLine className="h-4 w-4" />
                        </IconBtn>
                      )}
                      {perms.delete && (
                        <IconBtn label="Delete" onClick={() => setToDelete(e)} danger>
                          <Trash2 className="h-4 w-4" />
                        </IconBtn>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {dragging && perms.upload && (
        <div className="pointer-events-none absolute inset-2 z-30 flex items-center justify-center rounded-3xl border-2 border-dashed border-brand-500 bg-brand-500/10 backdrop-blur-[2px]">
          <div className="rounded-2xl bg-white px-6 py-4 text-center shadow-xl dark:bg-zinc-900">
            <Upload className="mx-auto mb-2 h-6 w-6 text-brand-500" />
            <div className="font-semibold">Drop to upload</div>
            <div className="text-sm text-zinc-500">to {crumbs[crumbs.length - 1] || session.name}</div>
          </div>
        </div>
      )}

      <Dialog
        open={mkdirOpen}
        onClose={() => setMkdirOpen(false)}
        title="New folder"
        footer={
          <>
            <Button variant="ghost" onClick={() => setMkdirOpen(false)}>Cancel</Button>
            <Button variant="primary" onClick={createFolder} disabled={!newName.trim()}>Create</Button>
          </>
        }
      >
        <TextInput autoFocus value={newName} onChange={(e) => setNewName(e.target.value)} onKeyDown={(e) => e.key === "Enter" && createFolder()} placeholder="Folder name" maxLength={255} />
      </Dialog>

      <Dialog
        open={!!toDelete}
        onClose={() => setToDelete(null)}
        title={`Delete “${toDelete?.name ?? ""}”?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setToDelete(null)}>Cancel</Button>
            <Button variant="danger" onClick={doDelete}>Delete</Button>
          </>
        }
      >
        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          {toDelete?.isDir ? "This folder and everything in it will be permanently deleted from the host's computer." : "This file will be permanently deleted from the host's computer."}
        </p>
      </Dialog>
    </div>
  );
}

function IconBtn({ children, label, onClick, danger }: { children: React.ReactNode; label: string; onClick: () => void; danger?: boolean }) {
  return (
    <button
      onClick={(e) => {
        e.stopPropagation();
        onClick();
      }}
      title={label}
      aria-label={label}
      className={cx("rounded-lg p-2 text-zinc-500 transition hover:bg-zinc-100 dark:hover:bg-zinc-800", danger ? "hover:text-red-600" : "hover:text-zinc-900 dark:hover:text-white")}
    >
      {children}
    </button>
  );
}

function Empty({ title, text }: { title: string; text: string }) {
  return (
    <div className="sb-fade-in flex flex-col items-center justify-center px-6 py-24 text-center">
      <div className="mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-zinc-100 dark:bg-zinc-900">
        <FileIcon name="" isDir className="h-7 w-7" />
      </div>
      <div className="font-semibold">{title}</div>
      <div className="mt-1 max-w-sm text-sm text-zinc-500">{text}</div>
    </div>
  );
}

// Walks dropped folders (Chrome, Edge, Firefox and Safari support entries).
async function collectDropped(dt: DataTransfer): Promise<UploadItem[]> {
  const out: UploadItem[] = [];
  const entries = Array.from(dt.items)
    .map((i) => (i.kind === "file" ? i.webkitGetAsEntry?.() : null))
    .filter(Boolean) as FileSystemEntry[];
  if (!entries.length) return Array.from(dt.files, (file) => ({ file, rel: "" }));
  const walk = async (e: FileSystemEntry, prefix: string): Promise<void> => {
    if (e.isFile) {
      const file = await new Promise<File>((res, rej) => (e as FileSystemFileEntry).file(res, rej));
      out.push({ file, rel: prefix ? `${prefix}/${file.name}` : "" });
    } else if (e.isDirectory) {
      const reader = (e as FileSystemDirectoryEntry).createReader();
      const path = prefix ? `${prefix}/${e.name}` : e.name;
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej));
        if (!batch.length) break;
        for (const c of batch) await walk(c, path);
      }
    }
  };
  for (const e of entries) await walk(e, "");
  return out;
}
