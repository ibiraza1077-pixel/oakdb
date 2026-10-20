// An in-memory file system with the subset of Node's fs API that Go's js/wasm
// runtime calls (see $GOROOT/src/syscall/fs_js.go). OakDB opens oak.db and
// oak.db-wal through os.OpenFile as usual; here they are byte arrays in memory.
// Load this before wasm_exec.js, which only installs its own stub when
// globalThis.fs is missing.
(() => {
  const C = { O_WRONLY: 1, O_RDWR: 2, O_CREAT: 64, O_EXCL: 128, O_TRUNC: 512, O_APPEND: 1024, O_DIRECTORY: 65536 };
  const files = new Map(); // path -> { data: Uint8Array, size, mtime, ino }
  const fds = new Map(); // fd -> { file, pos, flags, dir }
  let nextFd = 10;
  let nextIno = 2;
  const decoder = new TextDecoder();
  let console$ = '';

  const fail = (code) => Object.assign(new Error(code), { code });
  const norm = (p) => (p.startsWith('/') ? p : '/' + p).replace(/\/+/g, '/');

  function grow(f, need) {
    if (need <= f.data.length) return;
    const next = new Uint8Array(Math.max(need, f.data.length * 2, 1 << 16));
    next.set(f.data.subarray(0, f.size));
    f.data = next;
  }

  function truncate(f, len) {
    grow(f, len);
    if (len < f.size) f.data.fill(0, len, f.size);
    f.size = len;
    f.mtime = Date.now();
  }

  function stat(f, dir = false) {
    const t = f ? f.mtime : Date.now();
    return {
      dev: 1, ino: f ? f.ino : 1, mode: dir ? 0o40755 : 0o100644, nlink: 1, uid: 0, gid: 0, rdev: 0,
      size: f ? f.size : 0, blksize: 4096, blocks: f ? Math.ceil(f.size / 512) : 0,
      atimeMs: t, mtimeMs: t, ctimeMs: t,
      isDirectory: () => dir,
    };
  }

  function writeAt(fd, buf, off, len, pos) {
    const h = fds.get(fd);
    if (!h || h.dir) throw fail('EBADF');
    const f = h.file;
    const at = pos ?? (h.flags & C.O_APPEND ? f.size : h.pos);
    grow(f, at + len);
    f.data.set(buf.subarray(off, off + len), at);
    f.size = Math.max(f.size, at + len);
    f.mtime = Date.now();
    if (pos == null) h.pos = at + len;
    return len;
  }

  function readAt(fd, buf, off, len, pos) {
    const h = fds.get(fd);
    if (!h || h.dir) throw fail('EBADF');
    const f = h.file;
    const at = pos ?? h.pos;
    const n = Math.max(0, Math.min(len, f.size - at));
    buf.set(f.data.subarray(at, at + n), off);
    if (pos == null) h.pos = at + n;
    return n;
  }

  // Wraps a synchronous implementation in Node's callback style.
  const cb = (fn) => (...args) => {
    const done = args.pop();
    let res;
    try { res = fn(...args); } catch (e) { done(e.code ? e : fail('EIO')); return; }
    done(null, res);
  };
  const ok = cb(() => undefined);

  globalThis.fs = {
    constants: C,

    // Go's runtime prints stdout/stderr through writeSync.
    writeSync(fd, buf) {
      if (fd === 1 || fd === 2) {
        console$ += decoder.decode(buf);
        const nl = console$.lastIndexOf('\n');
        if (nl !== -1) { console.log(console$.slice(0, nl)); console$ = console$.slice(nl + 1); }
        return buf.length;
      }
      return writeAt(fd, buf, 0, buf.length, null);
    },
    write: cb((fd, buf, off, len, pos) => (fd === 1 || fd === 2 ? globalThis.fs.writeSync(fd, buf.subarray(off, off + len)) : writeAt(fd, buf, off, len, pos))),
    read: cb(readAt),

    open: cb((path, flags) => {
      path = norm(path);
      if (path === '/' || flags & C.O_DIRECTORY) {
        if (path !== '/') throw fail('ENOTDIR');
        const fd = nextFd++;
        fds.set(fd, { dir: true, pos: 0, flags });
        return fd;
      }
      let f = files.get(path);
      if (f && flags & C.O_CREAT && flags & C.O_EXCL) throw fail('EEXIST');
      if (!f) {
        if (!(flags & C.O_CREAT)) throw fail('ENOENT');
        f = { data: new Uint8Array(0), size: 0, mtime: Date.now(), ino: nextIno++ };
        files.set(path, f);
      }
      if (flags & C.O_TRUNC) truncate(f, 0);
      const fd = nextFd++;
      fds.set(fd, { file: f, pos: 0, flags, dir: false });
      return fd;
    }),
    close: cb((fd) => { if (!fds.delete(fd)) throw fail('EBADF'); }),
    fstat: cb((fd) => {
      const h = fds.get(fd);
      if (!h) throw fail('EBADF');
      return h.dir ? stat(null, true) : stat(h.file);
    }),
    stat: cb((path) => {
      path = norm(path);
      if (path === '/') return stat(null, true);
      const f = files.get(path);
      if (!f) throw fail('ENOENT');
      return stat(f);
    }),
    lstat(path, done) { globalThis.fs.stat(path, done); },
    fsync: ok, // memory is as durable as it gets in a browser tab
    ftruncate: cb((fd, len) => {
      const h = fds.get(fd);
      if (!h || h.dir) throw fail('EBADF');
      truncate(h.file, len);
    }),
    truncate: cb((path, len) => {
      const f = files.get(norm(path));
      if (!f) throw fail('ENOENT');
      truncate(f, len);
    }),
    unlink: cb((path) => { if (!files.delete(norm(path))) throw fail('ENOENT'); }),
    rename: cb((from, to) => {
      const f = files.get(norm(from));
      if (!f) throw fail('ENOENT');
      files.delete(norm(from));
      files.set(norm(to), f);
    }),
    readdir: cb((path) => {
      if (norm(path) !== '/') throw fail('ENOTDIR');
      return [...files.keys()].map((k) => k.slice(1));
    }),
    mkdir: cb(() => { throw fail('EEXIST'); }),
    rmdir: cb(() => { throw fail('ENOTEMPTY'); }),
    chmod: ok, fchmod: ok, chown: ok, fchown: ok, lchown: ok, utimes: ok,
    link: cb(() => { throw fail('ENOSYS'); }),
    symlink: cb(() => { throw fail('ENOSYS'); }),
    readlink: cb(() => { throw fail('ENOSYS'); }),
  };

  // Lets the demo page look at, and deliberately damage, OakDB's files.
  globalThis.memfs = {
    size: (path) => files.get(norm(path))?.size ?? 0,
    // Cuts a file short, the way a crash mid-write leaves the last write torn.
    tear: (path, len) => {
      const f = files.get(norm(path));
      if (f) truncate(f, Math.min(len, f.size));
    },
  };
})();
