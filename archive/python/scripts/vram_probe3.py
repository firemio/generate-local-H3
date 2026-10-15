import os, sys, time, torch
from safetensors import safe_open
print("env:", {k: os.environ.get(k) for k in ("PYTORCH_HIP_ALLOC_CONF","GPU_MAX_HEAP_SIZE","GPU_MAX_ALLOC_PERCENT","HIP_VISIBLE_DEVICES")})
p = r"C:\firemio\generate-local-H3\models\text_encoders\qwen3vl_32b_minimax_h3_int8_convrot.safetensors"
tot=0; n=0; t=time.time(); keep=[]
try:
    with safe_open(p, framework="pt", device="cpu") as f:
        for k in f.keys():
            g = f.get_tensor(k).to("cuda"); keep.append(g); tot += g.numel()*g.element_size(); n += 1
            if tot > 20e9: break
except torch.OutOfMemoryError as e:
    print(f"  OOM at tensor #{n} after {tot/1e9:.2f} GB"); sys.exit(1)
print(f"  ok: {n} tensors {tot/1e9:.1f} GB in {time.time()-t:.0f}s")
