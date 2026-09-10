"""Sanity check for the ROCm PyTorch install on gfx1151 (run inside .venv)."""
import time
import torch

print("torch      :", torch.__version__)
print("hip        :", torch.version.hip)
print("available  :", torch.cuda.is_available())
if not torch.cuda.is_available():
    raise SystemExit("no HIP device visible (check CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES)")
p = torch.cuda.get_device_properties(0)
print("device     :", torch.cuda.get_device_name(0), getattr(p, "gcnArchName", ""))
print("total mem  : %.1f GB" % (p.total_memory / 1e9))
free, total = torch.cuda.mem_get_info()
print("free mem   : %.1f GB" % (free / 1e9))

for dt in (torch.float16, torch.bfloat16):
    a = torch.randn(4096, 4096, device="cuda", dtype=dt)
    for _ in range(3):
        a @ a
    torch.cuda.synchronize()
    t = time.time()
    n = 20
    for _ in range(n):
        a @ a
    torch.cuda.synchronize()
    dt_s = time.time() - t
    print("%s matmul 4096^3: %.1f TFLOPS" % (str(dt).split(".")[-1], n * 2 * 4096 ** 3 / dt_s / 1e12))

q = torch.randn(1, 8, 4096, 128, device="cuda", dtype=torch.bfloat16)
torch.cuda.synchronize(); t = time.time()
for _ in range(5):
    torch.nn.functional.scaled_dot_product_attention(q, q, q)
torch.cuda.synchronize()
print("sdpa bf16 (1x8x4096x128) x5: %.3fs" % (time.time() - t))
print("flash sdpa available:", torch.backends.cuda.is_flash_attention_available())
print("OK")
