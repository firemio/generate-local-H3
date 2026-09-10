# AIテレビ局 (aitv)

「ローカル LLM が番組を書き、MiniMax H3 が映像と音声（日本語のセリフ込み）を生成し、
ブラウザ／HLS で流し続ける」放送局。外部 API は一切使わない。

```
             ┌───────────────┐   番組表(重み付き乱択)   ┌──────────────────────┐
             │ aitv.programs │ ───────────────────────▶ │  aitv.producer        │
             │  news/weather │                          │  LLM(Ollama/LM Studio)│─┐ H3 プロンプト(JSON)
             │  nature/cm/.. │                          │  → h3gen → ComfyUI    │ │
             └───────────────┘                          │  → ffmpeg normalize   │◀┘
                                                        └──────────┬───────────┘
                                                                   │ aitv/library/*.mp4 + index.json
                                     ┌─────────────────────────────┼──────────────────────────┐
                                     ▼                             ▼                          ▼
                            aitv.server (:8800)              aitv.hls (ffmpeg)          任意の再生ソフト
                            /            プレイヤー           aitv/hls/live.m3u8         VLC / OBS で視聴
                            /playlist.json                    (連続ライブ配信)
                            /clips/<file>
```

## 使い方

```powershell
# 1. ComfyUI を起動（別ターミナル）
.\scripts\run_comfyui.ps1
# 2. ローカル LLM（どちらか）
ollama serve            # gemma4:latest を既定で使用
# LM Studio なら: サーバを起動して --llm-url http://127.0.0.1:1234/v1 --llm-model <model>
# 3. 放送局を起動: Web プレイヤー + HLS + 制作ループ
.\.venv\Scripts\python.exe -m aitv station --port 8800
# → http://127.0.0.1:8800/  （HLS: http://127.0.0.1:8800/hls/live.m3u8）
```

個別に:

```powershell
python -m aitv produce --count 3 --formats news,cm,nature   # 3番組だけ制作
python -m aitv produce --no-llm                              # LLM 無し（内蔵の固定台本）
python -m aitv serve                                         # プレイヤーだけ
python -m aitv hls                                           # HLS 配信だけ
```

## 番組フォーマット（`aitv/programs.py`）

| key | 番組 | セグメント | 内容 |
|---|---|---|---|
| news | AIニュース | 3×6s | アンカーが日本語で架空のニュースを読む |
| weather | お天気 | 2×6s | 天気図の前で予報、屋外カット＋ナレーション |
| nature | 自然ドキュメンタリー | 3×8s | 環境音＋日本語ナレーション |
| cm | CM | 1×6s | 架空商品の CM、日本語キャッチコピー |
| cooking | 3分クッキング | 2×6s | シェフのトーク＋調理音 |
| music | ミュージックブレイク | 1×8s | セリフ無し、音楽指定 |
| ident | ステーションID | 1×4s | マスコット＋ジングル |

LLM には H3 公式のプロンプト構造（`integrated_multimodal_description` / `overall_soundscape` /
`non_diegetic_music`、セリフは `<d>[Japanese] …</d>`、話者は `(S1)`）を system prompt で教え、
JSON で `{title, segments:[{caption, seconds, prompt}]}` を返させる。LLM が落ちていれば内蔵台本にフォールバック。

## 24 時間運用と「同じ番組が回る」問題

```powershell
.\scripts\run_station.ps1            # ComfyUI（未起動なら）+ プレイヤー + HLS + 制作ループ（無限）を常駐起動
.\scripts\run_station.ps1 -Restart   # 作り直し
.\scripts\run_station.ps1 -Stop
```

制作速度は実時間の約 1/60（832×480 で 6.6 秒のクリップに約 400 秒）。つまり **1 日回しても新作は約 25 分ぶん**。
放送局として 24 時間止めないことと、24 時間ぶんの新作が毎日できることは別で、後者はこの GPU では不可能。
そこで配信側は次の並び順で「同じループ」に見えないようにしている（`aitv/hls.py: pick_next`、プレイヤーも同じ方針）:

1. まだ一度も流していないクリップがあれば、新しい順に即座に流す（できたての番組が最優先で電波に乗る）。
2. 無ければ、直近に流したものを避けつつ、新しいクリップほど重みを高くしたランダム選択。

繰り返し感をさらに減らすには: 640×352 に落として制作を 2.8 倍速くする（`-Width 640 -Height 352`）、番組フォーマットを増やす、
数日回してライブラリを育てる（ライブラリは消さない限り増え続ける）。

## 連続運転のための作り（レビューで直したもの）

数日回す前提でコードを見直し、確認できた不具合を直した。

| 症状 | 原因 | 対処 |
|---|---|---|
| 配信が止まったまま復帰しない | HLS エンコーダが死ぬと、remux 側が誰も読まない stdout パイプに書き込んだままブロックし、`remux.wait()` が永久に返らない | 壊れたら remux を kill してから待つ。エンコーダは自動で作り直す |
| 再生時刻がおかしい | `-fflags +igndts` が正しい DTS を捨て、クリップ境界以降のタイムスタンプが 1 tick 刻みに潰れる（実測 71 % のフレーム） | `+genpts` のみに。1/24 秒刻みに戻ることを ffprobe で確認 |
| 生成したクリップが消える | `index.json` を `os.replace` で置き換える際、他スレッドが開いていると Windows は PermissionError。約 400 秒かけた生成が索引に載らない | `aitv/index_store.py` に集約。読み手は閉じてから parse、書き手はリトライ＋最後は上書き |
| 生成が永久に止まる | HIP がハングしても `/system_stats` は応答し続けるので、生死判定だけでは検知できない | 無反応タイムアウト（既定 20 分）で `/interrupt` と待ち行列削除。失敗が続けば ComfyUI 再起動 |
| ディスクが減り続ける | ComfyUI の生出力（1 クリップ約 1.4 MB、1 日約 200 本）を誰も消さない | 正規化してライブラリに入れた後で削除 |
| 二重起動 | Windows の SO_REUSEADDR は listen 中のポートにも bind できてしまう | Windows では無効化。ランチャも PID ファイルで単一起動を保証 |
| 停止で無関係なプロセスまで落ちる | コマンドラインの `-m aitv` 部分一致で kill していた（実際に別のシェルを巻き込んだ） | PID ファイル＋`taskkill /T` に変更 |
| ログ出力で落ちる | cp932 では日本語キャプションを encode できず、リダイレクト時に UnicodeEncodeError | 起動時に stdout/stderr を UTF-8 に再構成 |

`.\scriptsun_station.ps1 -Status` で稼働状況、`-Stop` で停止、`-Stop -All` で ComfyUI も停止。

## 設計メモ

- **1 クリップ = 1 生成**。H3 は 4〜15 秒の生成が前提なので、番組は短いセグメントの連結として設計。
  同一人物の継続性は Ref2VA（参照画像＋声）で実現できる（`h3gen gen --mode r2v`）。
- **正規化**: 生成物は ffmpeg で 24fps / H.264 / AAC 48kHz に揃えて `aitv/library/` に置く。
  HLS ブロードキャスタはストリームコピーで TS 化して連結するので再エンコード不要。
- **プレイヤー**は `playlist.json` を 30 秒ごとに再取得し、未再生クリップを優先して順に再生。
  ライブラリが空の間はカラーバー表示。
- **HLS** は ffmpeg 1 プロセスに stdin から TS を流し込み続ける方式（`-re` で実時間ペース）。
  ライブラリに追加されたクリップは次の周回で自動的に流れる。
