from huggingface_hub import HfApi

files = HfApi().list_repo_files("cyankiwi/Ornith-1.0-9B-AWQ-INT4", repo_type="model")
print(f"HF_REPO_FILES={len(files)}")
print("HF_SHARDS=" + ",".join(name for name in files if name.endswith(".safetensors")))
