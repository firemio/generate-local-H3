# Third-party notices

このリポジトリ本体のコードは MIT（[LICENSE](LICENSE)）ですが、以下は別の権利者・条件のものです。

## 同梱しているファイル

### `workflows/templates/*.json`

ComfyUI 公式のワークフローテンプレート。参照用にそのまま置いています。

- 出典: [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates)
- ライセンス: MIT License, Copyright (c) Comfy Org

### `input/*.png`, `report/assets/*.mp4`, `report/assets/*.jpg`, `report/assets/*.png`

MiniMax H3 で生成した動画・静止画と、そのコンタクトシートです。**MIT の対象外**で、
モデル提供元の条件に従います。

- モデル: [MiniMaxAI/MiniMax-H3](https://huggingface.co/MiniMaxAI/MiniMax-H3)
- ライセンス: MiniMax H3 Community License Agreement

## セットアップ時に取得するもの（このリポジトリには含まれません）

| 対象 | 取得元 | ライセンス |
|---|---|---|
| ComfyUI | [comfyanonymous/ComfyUI](https://github.com/comfyanonymous/ComfyUI) | GPL-3.0 |
| MiniMax H3 の重み | [Comfy-Org/MiniMax-H3](https://huggingface.co/Comfy-Org/MiniMax-H3) | MiniMax H3 Community License Agreement |
| PyTorch (ROCm, gfx1151) | [repo.amd.com/rocm/whl/gfx1151](https://repo.amd.com/rocm/whl/gfx1151/) | BSD-3-Clause + AMD ROCm の各ライセンス |
| comfy-kitchen ほか ComfyUI の依存 | PyPI | 各パッケージの表示に従う |

`scripts/setup.ps1` と `scripts/download_models.ps1` はこれらを取得するだけで、再配布はしません。
MiniMax H3 の重みを使うには、配布元の利用条件（地域によっては申請が必要）を各自で確認してください。

## 参考にした資料

- 松尾公也「[MiniMax H3 高速化プロジェクト](https://www.techno-edge.net/article/2026/09/06/5468.html)」テクノエッジ, 2026-09-06
- [matsuo-koya/minimax-h3-notes](https://github.com/matsuo-koya/minimax-h3-notes) (MIT)
