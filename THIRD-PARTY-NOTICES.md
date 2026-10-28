# Third-party notices

本仓库本身的代码采用 MIT 许可（[LICENSE](LICENSE)），但以下内容属于其他权利人与条件。

## 随仓库分发的文件

### `workflows/templates/*.json`

ComfyUI 官方工作流模板。为便于参照而原样放置。

- 出处: [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates)
- 许可证: MIT License, Copyright (c) Comfy Org

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

用 MiniMax H3 生成的视频·图片及其缩略拼图（contact sheet）。**不属于 MIT 许可范围**，
遵循模型提供方的条件。

- 模型: [MiniMaxAI/MiniMax-H3](https://huggingface.co/MiniMaxAI/MiniMax-H3)
- 许可证: MiniMax H3 Community License Agreement

## 安装时获取的内容（不包含在本仓库中）

| 对象 | 获取来源 | 许可证 |
|---|---|---|
| ComfyUI | [comfyanonymous/ComfyUI](https://github.com/comfyanonymous/ComfyUI) | GPL-3.0 |
| MiniMax H3 权重 | [Comfy-Org/MiniMax-H3](https://huggingface.co/Comfy-Org/MiniMax-H3) | MiniMax H3 Community License Agreement |
| PyTorch (ROCm, gfx1151) | [stable.repo.amd.com/rocm/whl-next](https://stable.repo.amd.com/rocm/whl-next/)（`torch[device-gfx1151]`） | BSD-3-Clause + AMD ROCm 各项许可 |
| comfy-kitchen 及其他 ComfyUI 依赖 | PyPI | 遵循各包自身的声明 |

`scripts/setup.ps1` 与 `scripts/download_models.ps1` 只负责获取这些内容，不做再分发。
使用 MiniMax H3 权重前，请自行确认分发方的使用条件（部分地区需要申请）。

## 参考资料

- 松尾公也《[MiniMax H3 提速项目](https://www.techno-edge.net/article/2026/09/06/5468.html)》，TechNode, 2026-09-06
- [matsuo-koya/minimax-h3-notes](https://github.com/matsuo-koya/minimax-h3-notes) (MIT)