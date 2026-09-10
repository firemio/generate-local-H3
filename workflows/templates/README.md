# ComfyUI 公式 H3 ワークフローテンプレート（参照用）

このディレクトリの `*.json` は [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates)
から取得したものをそのまま置いています（MIT License, Copyright (c) Comfy Org）。

`h3gen/graph.py` が組み立てる API 形式グラフの元になったノード構成を、比較できるように残してあります。
最新版は次で取り直せます。

```powershell
foreach ($n in "video_minimax_h3_t2v","video_minimax_h3_i2v","video_minimax_h3_r2v",
               "video_minimax_h3_i2v_continuation","video_minimax_h3_multiframe_reference") {
    Invoke-WebRequest "https://raw.githubusercontent.com/Comfy-Org/workflow_templates/main/templates/$n.json" -OutFile "$n.json"
}
```
