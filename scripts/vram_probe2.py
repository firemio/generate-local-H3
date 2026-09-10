"""Is the ceiling about allocation COUNT (Windows HIP handle limit) rather than bytes?"""
import sys, time, torch, gc
def probe(chunk_mb, cap_gb):
    bufs=[]; t=time.time()
    try:
        while len(bufs)*chunk_mb/1024 < cap_gb:
            bufs.append(torch.empty(int(chunk_mb*2**20), dtype=torch.uint8, device="cuda"))
    except torch.OutOfMemoryError as e:
        print(f"  chunk {chunk_mb} MB: OOM after {len(bufs)} allocs = {len(bufs)*chunk_mb/1024:.1f} GB  ({time.time()-t:.1f}s)")
        del bufs; gc.collect(); torch.cuda.empty_cache(); return
    print(f"  chunk {chunk_mb} MB: reached cap {cap_gb} GB with {len(bufs)} allocs, no OOM ({time.time()-t:.1f}s)")
    del bufs; gc.collect(); torch.cuda.empty_cache()
for mb in (64, 32, 126):
    probe(mb, 40)
print("=== safetensors load test (text encoder int8, tensor by tensor) ===")
from safetensors import safe_open
p = r"C:\firemio\generate-local-H3\models\text_encoders\qwen3vl_32b_minimax_h3_int8_convrot.safetensors"
tot=0; n=0; t=time.time(); keep=[]
try:
    with safe_open(p, framework="pt", device="cpu") as f:
        for k in f.keys():
            x = f.get_tensor(k)
            g = x.to("cuda", non_blocking=False)
            keep.append(g); tot += g.numel()*g.element_size(); n += 1
            if n % 200 == 0: print(f"  {n} tensors {tot/1e9:.1f} GB {time.time()-t:.0f}s", flush=True)
except torch.OutOfMemoryError as e:
    print(f"OOM at tensor #{n} ({k}) after {tot/1e9:.2f} GB: {str(e)[:120]}")
else:
    print(f"loaded all {n} tensors = {tot/1e9:.1f} GB in {time.time()-t:.0f}s")
