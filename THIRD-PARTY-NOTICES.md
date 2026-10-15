# Third-party notices

このリポジトリ本体のコードは MIT（[LICENSE](LICENSE)）ですが、以下は別の権利者・条件のものです。

## 同梱しているファイル

### `workflows/templates/*.json`

ComfyUI 公式のワークフローテンプレート。参照用にそのまま置いています。

- 出典: [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates)
- ライセンス: MIT License, Copyright (c) Comfy Org

### `aitv/web/hls.min.js`

放送局の Web プレイヤーが HLS を再生するためのライブラリ（v1.6.5 のビルド済みファイルをそのまま同梱）。

- 出典: [video-dev/hls.js](https://github.com/video-dev/hls.js)
- ライセンス: Apache License 2.0, Copyright (c) 2017 Dailymotion (and contributors)

### Go の依存（`go.mod`; ビルド時にモジュールキャッシュへ取得、ソースは同梱しない）

| モジュール | 用途 | ライセンス |
|---|---|---|
| [gorilla/websocket](https://github.com/gorilla/websocket) v1.5.3 | ComfyUI の `/ws` イベント購読 | BSD-2-Clause, Copyright (c) 2013 The Gorilla WebSocket Authors |
| [ikawaha/kagome](https://github.com/ikawaha/kagome) v2 | セリフの漢字→かな変換（形態素解析） | MIT, Copyright (c) 2020 ikawaha |
| [ikawaha/kagome-dict/ipa](https://github.com/ikawaha/kagome-dict) | 同上の辞書。mecab-ipadic-2.7.0-20070801 のデータを含む | MIT（パッケージ）+ mecab-ipadic の条件（奈良先端科学技術大学院大学 / 日本電信電話株式会社。NOTICE.txt を参照） |

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
