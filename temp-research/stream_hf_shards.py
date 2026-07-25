import os
import pathlib
import urllib.request

REPO = "cyankiwi/Ornith-1.0-9B-AWQ-INT4"
OUT = pathlib.Path(r"K:\projects\llm_models\models\ornith-awq-stream")
TOKEN = os.environ["HF_TOKEN"]
OUT.mkdir(parents=True, exist_ok=True)

for name in ("model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"):
    target = OUT / name
    url = f"https://huggingface.co/{REPO}/resolve/main/{name}?download=true"
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {TOKEN}"})
    done = 0
    with urllib.request.urlopen(req, timeout=300) as response, target.open("wb") as output:
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            output.write(chunk)
            done += len(chunk)
            if done % (64 * 1024 * 1024) < len(chunk):
                print(f"{name}: {done / 1e9:.2f} GB", flush=True)
    print(f"DONE {name}: {done} bytes", flush=True)
