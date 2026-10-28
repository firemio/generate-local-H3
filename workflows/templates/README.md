# ComfyUI 官方 H3 工作流模板（供参照）

本目录下的 `*.json` 是从 [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates)
原样取来的文件（MIT License, Copyright (c) Comfy Org）。

它们保留了 `h3gen/graph.py` 所组装的 API 格式图最初的节点构成，便于对比。
需要最新版时用下面的命令重新拉取。

```powershell
foreach ($n in "video_minimax_h3_t2v","video_minimax_h3_i2v","video_minimax_h3_r2v",
               "video_minimax_h3_i2v_continuation","video_minimax_h3_multiframe_reference") {
    Invoke-WebRequest "https://raw.githubusercontent.com/Comfy-Org/workflow_templates/main/templates/$n.json" -OutFile "$n.json"
}
```