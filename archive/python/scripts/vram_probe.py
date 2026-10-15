"""Find how much HIP device memory a single process can actually allocate (Strix Halo / Windows)."""
import os, sys, time, torch
conf = os.environ.get("PYTORCH_HIP_ALLOC_CONF", "") or os.environ.get("PYTORCH_CUDA_ALLOC_CONF", "")
print("alloc conf:", repr(conf))
free, total = torch.cuda.mem_get_info()
print("mem_get_info free/total GB: %.1f / %.1f" % (free/1e9, total/1e9))
chunk_gb = float(sys.argv[1]) if len(sys.argv) > 1 else 1.0
bufs = []
t = time.time()
try:
    while True:
        bufs.append(torch.empty(int(chunk_gb * 1e9), dtype=torch.uint8, device="cuda"))
        if len(bufs) % 8 == 0:
            print("  allocated %.0f GB" % (len(bufs) * chunk_gb), flush=True)
        if len(bufs) * chunk_gb >= 100:
            break
except torch.OutOfMemoryError as e:
    print("OOM after %.1f GB: %s" % (len(bufs) * chunk_gb, str(e)[:160]))
print("ceiling ~ %.1f GB in %.1fs" % (len(bufs) * chunk_gb, time.time() - t))
if bufs:
    bufs[0].fill_(1); torch.cuda.synchronize(); print("touch ok")
