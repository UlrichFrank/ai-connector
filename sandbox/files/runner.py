"""The entrypoint of the files sandbox.

Reads one JSON request from stdin, runs it (model-written Python, or the built-in Markdown to PDF) as a child
process with a wall-clock limit, then writes a tar to stdout: first result.json (exit code, log tail, files,
skipped files), then the produced files under out/. The host checks everything again (names, types, sizes, magic
bytes): this side only pre-filters so that nothing large is streamed for nothing.

The child's stdout/stderr go to a log file, never to our stdout (the tar stream).
"""

import ctypes
import io
import json
import os
import shutil
import signal
import subprocess
import sys
import tarfile
import time

OUT = "/out"
JOB = "/tmp/job"
LOG_TAIL = 4000


def no_dump():
    # make our /proc/<pid>/fd unreadable for the (same-uid) child: it cannot write into the tar stream
    try:
        ctypes.CDLL(None, use_errno=True).prctl(4, 0, 0, 0, 0)  # PR_SET_DUMPABLE = 4
    except Exception:
        pass


def tail(path, n):
    try:
        with open(path, "rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            f.seek(max(0, size - n))
            data = f.read()
        text = data.decode("utf-8", "replace")
        if size > n:
            text = "…" + text
        return text
    except OSError:
        return ""


def run_child(argv, env, timeout):
    log = open(os.path.join(JOB, "log.txt"), "wb")
    start = time.monotonic()
    p = subprocess.Popen(
        argv,
        cwd=OUT,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=log,
        stderr=subprocess.STDOUT,
        start_new_session=True,
        close_fds=True,
    )
    timed_out = False
    try:
        code = p.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except OSError:
            pass
        code = p.wait()
    log.close()
    # anything the child left running in its session goes too
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except OSError:
        pass
    return code, timed_out, time.monotonic() - start


def mpl_config():
    # matplotlib wants a writable config dir: copy the font cache warmed at build time to /tmp (a tmpfs)
    dst = "/tmp/mpl"
    try:
        shutil.copytree("/opt/sandbox/mpl", dst, dirs_exist_ok=True)
    except OSError:
        os.makedirs(dst, exist_ok=True)
    return dst


def pdf_pages(path):
    try:
        import pypdf

        return len(pypdf.PdfReader(path).pages)
    except Exception:
        return None


def collect(allowed, max_bytes, max_files):
    files, skipped = [], []
    for root, dirs, names in os.walk(OUT, followlinks=False):
        dirs.sort()
        for name in sorted(names):
            path = os.path.join(root, name)
            rel = os.path.relpath(path, OUT)
            st = os.lstat(path)
            if not (st.st_mode & 0o170000 == 0o100000):  # regular files only (no symlinks, fifos, …)
                skipped.append({"name": rel, "reason": "not a regular file"})
                continue
            ext = name.rsplit(".", 1)[-1].lower() if "." in name else ""
            if ext not in allowed:
                skipped.append({"name": rel, "reason": "type ." + ext + " not allowed" if ext else "no extension"})
                continue
            if st.st_size > max_bytes:
                skipped.append({"name": rel, "reason": "too large (%d bytes, max %d)" % (st.st_size, max_bytes)})
                continue
            if st.st_size == 0:
                skipped.append({"name": rel, "reason": "empty"})
                continue
            if len(files) >= max_files:
                skipped.append({"name": rel, "reason": "too many files (max %d)" % max_files})
                continue
            f = {"name": rel, "size": st.st_size}
            if ext == "pdf":
                f["pages"] = pdf_pages(path)
            files.append(f)
    return files, skipped


def main():
    no_dump()
    req = json.load(sys.stdin)
    os.makedirs(JOB, exist_ok=True)
    timeout = float(req.get("timeout", 60))
    filename = req.get("filename") or "output"
    env = {
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "HOME": "/tmp",
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "PYTHONUNBUFFERED": "1",
        "PYTHONDONTWRITEBYTECODE": "1",
        "MPLBACKEND": "Agg",
        "MPLCONFIGDIR": mpl_config(),
        "XDG_CACHE_HOME": "/tmp/cache",
        "OUT_DIR": OUT,
        "OUT_FILE": os.path.join(OUT, filename),
    }

    mode = req.get("mode")
    if mode == "python":
        script = os.path.join(JOB, "main.py")
        with open(script, "w", encoding="utf-8") as f:
            f.write(req.get("code", ""))
        argv = ["python3", script]
    elif mode == "markdown":
        doc = os.path.join(JOB, "doc.json")
        with open(doc, "w", encoding="utf-8") as f:
            json.dump({"markdown": req.get("markdown", ""), "title": req.get("title", ""), "out": env["OUT_FILE"]}, f)
        argv = ["python3", "/opt/sandbox/markdown_pdf.py", doc]
    else:
        argv = ["python3", "-c", "raise SystemExit('unknown mode')"]

    code, timed_out, secs = run_child(argv, env, timeout)
    if timed_out:
        files, skipped = [], []
    else:
        files, skipped = collect(
            set(req.get("allowed", [])), int(req.get("max_bytes", 25 << 20)), int(req.get("max_files", 10))
        )

    result = {
        "exit": code,
        "timedOut": timed_out,
        "seconds": round(secs, 2),
        "log": tail(os.path.join(JOB, "log.txt"), LOG_TAIL),
        "files": files,
        "skipped": skipped,
    }

    out = sys.stdout.buffer
    with tarfile.open(fileobj=out, mode="w|", format=tarfile.PAX_FORMAT) as tar:
        data = json.dumps(result).encode()
        info = tarfile.TarInfo("result.json")
        info.size = len(data)
        tar.addfile(info, io.BytesIO(data))
        for f in files:
            path = os.path.join(OUT, f["name"])
            info = tarfile.TarInfo("out/" + f["name"])
            with open(path, "rb") as fh:
                st = os.fstat(fh.fileno())
                info.size = min(st.st_size, f["size"])
                tar.addfile(info, fh)
    out.flush()


if __name__ == "__main__":
    main()
