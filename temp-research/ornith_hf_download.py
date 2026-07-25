from huggingface_hub import snapshot_download

snapshot_download(
    "cyankiwi/Ornith-1.0-9B-AWQ-INT4",
    cache_dir="/root/.cache/huggingface",
    token=True,
)
