import concurrent.futures
import json
import os
import pathlib
import sys
import threading
import time

import requests

REPO = "cyankiwi/Ornith-1.0-9B-AWQ-INT4"
BASE = f"https://hf-mirror.com/{REPO}/resolve/main/"
OUT = pathlib.Path(r"K:\projects\llm_models\models\ornith-awq-manual")
CHUNK = 1 * 1024 * 1024
WORKERS = 64
TOKEN = os.environ.get("HF_TOKEN")
if not TOKEN:
    raise SystemExit("HF_TOKEN is not set")

HEADERS = {"Authorization": f"Bearer {TOKEN}"}
OUT.mkdir(parents=True, exist_ok=True)
print(f"output={OUT}", flush=True)

meta = requests.get(f"https://huggingface.co/api/models/{REPO}", headers=HEADERS, timeout=30)
meta.raise_for_status()
files = [x["rfilename"] for x in meta.json().get("siblings", [])]
weight_files = [x for x in files if x.endswith(".safetensors")]
small_files = [x for x in files if x not in weight_files and not x.startswith(".git/")]

def download_small(name):
    target = OUT / name
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.exists() and target.stat().st_size:
        return name
    r = requests.get(BASE + name, headers=HEADERS, timeout=(30, 120))
    r.raise_for_status()
    target.write_bytes(r.content)
    return name

for name in small_files:
    try:
        download_small(name)
        print(f"metadata {name}", flush=True)
    except Exception as exc:
        print(f"metadata-failed {name}: {exc}", flush=True)

def get_size(name):
    r = requests.head(BASE + name, headers=HEADERS, allow_redirects=True, timeout=30)
    r.raise_for_status()
    return int(r.headers["content-length"]), r.url

lock = threading.Lock()

def download_weight(name):
    target = OUT / name
    part = target.with_suffix(target.suffix + ".part")
    done_path = target.with_suffix(target.suffix + ".done.json")
    size, url = get_size(name)
    if not part.exists():
        with part.open("wb") as f:
            f.truncate(size)
    elif part.stat().st_size != size:
        with part.open("r+b") as f:
            f.truncate(size)
    done = set()
    if done_path.exists():
        try:
            done = set(json.loads(done_path.read_text()))
        except Exception:
            done = set()
    ranges = [(start, min(start + CHUNK, size)) for start in range(0, size, CHUNK)]
    pending = [x for x in ranges if x[0] not in done]
    print(f"weight {name} size={size} chunks={len(ranges)} pending={len(pending)}", flush=True)

    def one(item):
        start, end = item
        for attempt in range(1, 9):
            try:
                h = dict(HEADERS)
                h["Range"] = f"bytes={start}-{end - 1}"
                r = requests.get(url, headers=h, timeout=(10, 20))
                if r.status_code != 206:
                    raise RuntimeError(f"HTTP {r.status_code}")
                content = r.content
                if len(content) != end - start:
                    raise RuntimeError(f"short range {len(content)} expected {end-start}")
                with part.open("r+b") as f:
                    f.seek(start)
                    f.write(content)
                with lock:
                    done.add(start)
                    done_path.write_text(json.dumps(sorted(done)))
                    if len(done) % 8 == 0 or len(done) == len(ranges):
                        print(f"progress {name} {len(done)}/{len(ranges)}", flush=True)
                return
            except Exception as exc:
                if attempt == 8:
                    raise
                time.sleep(min(2 * attempt, 15))
                print(f"retry {name} offset={start} attempt={attempt}: {exc}", flush=True)

    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as pool:
        list(pool.map(one, pending))
    part.replace(target)
    done_path.unlink(missing_ok=True)
    print(f"complete {name}", flush=True)

for name in weight_files:
    download_weight(name)

print("ALL_COMPLETE", flush=True)
