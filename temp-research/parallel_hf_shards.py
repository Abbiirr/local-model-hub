import concurrent.futures
import os
import pathlib
import threading
import time
import urllib.request

REPO = "cyankiwi/Ornith-1.0-9B-AWQ-INT4"
OUT = pathlib.Path(r"K:\projects\llm_models\models\ornith-awq-direct")
TOKEN = os.environ["HF_TOKEN"]
CHUNK = 64 * 1024 * 1024
WORKERS = 8
URL_BASE = f"https://huggingface.co/{REPO}/resolve/main/"

OUT.mkdir(parents=True, exist_ok=True)
progress_lock = threading.Lock()


def request(url, headers):
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=180)


def file_size(name):
    url = URL_BASE + name + "?download=true"
    with request(url, {"Authorization": f"Bearer {TOKEN}", "Range": "bytes=0-0"}) as resp:
        content_range = resp.headers.get("Content-Range", "")
        if "/" in content_range:
            return int(content_range.rsplit("/", 1)[1])
        length = resp.headers.get("Content-Length")
        if length:
            return int(length)
    raise RuntimeError(f"could not determine size for {name}")


def download(name):
    target = OUT / name
    total = file_size(name)
    with target.open("wb") as f:
        f.truncate(total)
    completed = 0
    started = time.time()

    def fetch(start):
        nonlocal completed
        end = min(total - 1, start + CHUNK - 1)
        url = URL_BASE + name + "?download=true"
        with request(url, {"Authorization": f"Bearer {TOKEN}", "Range": f"bytes={start}-{end}"}) as resp:
            if resp.status != 206:
                raise RuntimeError(f"{name} range {start}-{end} returned HTTP {resp.status}")
            data = resp.read()
        if len(data) != end - start + 1:
            raise RuntimeError(f"{name} range {start}-{end} returned {len(data)} bytes")
        with target.open("r+b") as f:
            f.seek(start)
            f.write(data)
        with progress_lock:
            completed += len(data)
            elapsed = max(time.time() - started, 0.1)
            print(f"{name}: {completed / 1e9:.2f}/{total / 1e9:.2f} GB, {completed / elapsed / 1e6:.2f} MB/s", flush=True)

    ranges = range(0, total, CHUNK)
    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as pool:
        list(pool.map(fetch, ranges))
    print(f"DONE {name} {target}", flush=True)


for shard in ("model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"):
    download(shard)
