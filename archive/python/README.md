# archive/python — the first implementation (no longer run)

The station (`aitv/`) and the generation CLI (`h3gen/`) were first written in Python and ran the channel
from 2026-09-10 to 2026-09-17. They were then ported to Go (`cmd/`, `internal/`; contract in
[docs/go-port.md](../../docs/go-port.md)) and the Go binaries took over on 2026-09-17, resuming the
programme the Python producer was in the middle of. These files are kept for reference only:

- `aitv/` — the station: producer loop, two-pass writer, JMA/NHK sources, index store, rundown, HLS
  broadcaster, web server, loudness normalisation (`python -m aitv station`).
- `h3gen/` — the ComfyUI client, the H3 API-graph builder and the `gen|bench|info` CLI.
- `scripts/` — `make_report.py` (built `report/`), `vram_probe*.py` (the ROCm allocator probes behind
  `docs/pitfalls.md`).

They import each other with repository-root-relative paths and expect the data under `aitv/library`,
so they do not run from this directory as they are. `scripts/check_gpu.py` stays in `scripts/` because it
probes the ComfyUI venv's torch install, which is still Python.
