# 調査記録

実装の前と途中で確認した資料と、そこから**実装に採用した点**の記録。

- 確認日: **2026-11-16**（すべて同日）
- 確認方法: ページの取得（一部は実ブラウザ）、公式リポジトリのソース、実際のコマンド実行
- 「ソースで確認」は、ドキュメントに書かれていなかった点を、該当バージョンのソースコードで確かめたもの
- 「実機で確認」は、このリポジトリのコンテナイメージで実際に動かして確かめたもの

UI の参考ページは、情報の並べ方と操作の流れを見るために使った。画面の意匠・文言・画像は流用していない。

## 1. UI の参考

### Polycam Object Mode

<https://learn.poly.cam/hc/en-us/articles/27425185907348-How-to-Use-Object-Mode>
（プログラムからの取得は 403。実ブラウザで閲覧）

分かったこと:

- 1 回の撮影から「フォトグラメトリのメッシュ」と「Gaussian Splat」の 2 種類を作れる。
- 撮り方の指針: **最低 20 枚**、連続する写真の重なり **70〜75%**、**15〜30° ごと**に撮る、
  目の高さ・見下ろし・見上げの**複数の周回**、均一でやわらかい光、反射・透明・無地は苦手。
- シャッターは Auto / Manual / Timer の 3 種類。撮影中に点群のプレビューを出すモードもある（端末の姿勢推定が前提）。

採用した点:

- 推奨枚数を 20 枚とし、未満のときは「開始はできるが足した方がよい」と案内（`web/src/capture/validation.ts`）。
- 撮り方の説明を「何をするか」と「なぜか」の組にした（`web/src/pages/CapturePage.tsx` の CaptureGuide）。
- MVP は手動シャッターのみ。自動撮影は、端末の動きを知る手段（ARCore 等）が入ってから。
- メッシュと 3DGS を同じ撮影から両方作り、片方だけ成功した場合も見られるようにした。

### Scaniverse（Getting Started / Scan Techniques）

<https://www.nianticspatial.com/capture/scaniverse-getting-started>
<https://www.nianticspatial.com/docs/scaniverse/techniques/>

分かったこと:

- 流れは「撮影 → 確認 → アップロード（品質チェック）→ 生成 → 閲覧」。生成は数時間かかることもあり、
  アップロード中はアプリを開いたままにするよう案内している。
- 撮影の型: 対象を中心に一定の距離で周回、高さと傾きを変える、急な動きを避ける。
  繰り返し模様・模様の無い面・反射や透明・動く物は苦手。
- 端末の姿勢追跡や LiDAR を使った撮影中のメッシュ表示がある。

採用した点:

- 画面を「撮影 → 確認 → 送信 → 処理状況 → 閲覧」の順に 1 本道にした。
- 送信中は「画面を開いたままに」と表示。処理中は「閉じても続く。この URL で後から見られる」と表示。
- 撮影中のカバレッジ表示は、ブラウザだけでは端末の姿勢が取れないので見送った（VPS フェーズの課題）。

### RealityScan

<https://www.realityscan.com/>

製品紹介のページで、撮影の具体的な手順や数値は載っていなかった。
「撮影・確認・処理・共有」を 1 つのアプリで行うという構成だけを確認した。採用した点は特になし。

### PlayCanvas Model Viewer / SuperSplat Editor

<https://playcanvas.com/viewer> / <https://superspl.at/editor>（どちらも実ブラウザで閲覧）

分かったこと:

- どちらも表示領域が画面のほぼ全部を占め、暗い背景、操作部品は端にまとめてある。
- Model Viewer は `.glb` / `.gltf` / `.ply` をドラッグ＆ドロップで開く。床のグリッドが空間の手掛かりになっている。
- SuperSplat は編集用のパネルやツールバーが多い（MVP では編集しない）。

採用した点:

- ビューアは表示領域を最大にし、操作は「タブ・視点を戻す・保存」だけにした。
- 編集機能は入れない。ダウンロードした PLY / SOG は SuperSplat でそのまま開ける。

## 2. 技術の参考

### COLMAP

<https://colmap.github.io/cli.html> / <https://colmap.github.io/tutorial.html> /
<https://colmap.github.io/faq.html> / <https://colmap.github.io/install.html> /
<https://colmap.github.io/changelog.html> / <https://github.com/colmap/colmap>

分かったこと:

- 最新リリースは **4.2.1**（2026-09-29）。ライセンスは BSD。
- `mesh_texturer` は 4.0.0 で追加された。Ubuntu 24.04 の apt 版（3.9.1）には無い。
- CPU の指定は `--FeatureExtraction.use_gpu 0` と `--FeatureMatching.use_gpu 0`（3.9 系とは名前が違う）。
- FAQ: 「CUDA 対応 GPU が無ければ、**dense 再構成以外**のすべての機能が使える」。
  GPU 版の SIFT は OpenGL の画面を必要とするので、サーバーでは CPU 版が推奨。
- 公式の Docker イメージは CUDA 前提。CPU 専用のイメージは提供されていない。

ソースで確認（タグ 4.2.1）:

- `mesh_texturer` の引数は `--workspace_path`（歪み補正済みの作業場）、`--input_path`（PLY メッシュ）、
  `--output_path`（ディレクトリ）。出力は **`mesh.ply`（面ごとの UV つき）と `texture.png`**。OBJ は出力しない
  （`src/colmap/exe/mvs.cc`、`src/colmap/util/ply.cc`）。
- `delaunay_mesher` は `--input_type sparse` で疎なモデルを入力にできる。
- `colmap version` は `COLMAP 4.2.1 (Commit ... with CUDA | with HIP | without GPU support, ...)` と表示する
  （`src/colmap/util/version.cc.in`）。
- LSD（線分検出）は AGPL。CMake の `LSD_ENABLED=OFF` で外せる。

実機で確認:

- CPU 専用ビルドで SfM・`image_undistorter`・`delaunay_mesher`（疎）・`mesh_texturer` が動く。
- `mapper` は写真がつながらないと `Failed to create any sparse model` を出して終了コード 1 で終わる。
- 3D 点が極端に少ない入力（53 点）でメッシュ化が `SIGABRT` で落ちることがある。
- SIGTERM では止まらず、SIGKILL が必要だった。

採用した点:

- 4.2.1 をソースからビルド（`CUDA_ENABLED=OFF`、`GUI_ENABLED=OFF`、`ONNX_ENABLED=OFF`、`LSD_ENABLED=OFF`、
  `DOWNLOAD_ENABLED=OFF`）。`deploy/docker/Dockerfile`。
- GPU の有無を `colmap version` の文字列で判定し、無ければ疎な点群からの簡易メッシュに切り替える。
- テクスチャ付き PLY を Go で OBJ + MTL に変換する（`server/internal/reconstruction/ply.go`、`obj.go`）。
- 上記の失敗を、利用者向けの説明に変換する（`pipeline.go` の `userMessage`、`noSparseModel`）。

### OpenSplat

<https://github.com/WebODM/OpenSplat>（README、`opensplat.cpp`、`colmap.cpp`、`CMakeLists.txt`、LICENSE）

分かったこと:

- 最新は **v1.2.2**。ライセンスは **AGPL-3.0**。
- 入力: プロジェクトのルート。直下の `images/` と `sparse/0/{cameras,images,points3D}.bin` を読む
  （バイナリ形式のみ。ルート直下に `cameras.bin` があればそちらを使う）。
- 対応するカメラモデル: SIMPLE_PINHOLE / PINHOLE / SIMPLE_RADIAL / OPENCV / FULL_OPENCV。RADIAL は非対応。
- 主なオプション: `-n`（反復回数、既定 30000）、`-d`（縮小率）、`-o`（出力先。省略すると入力の隣）、`--cpu`。
- GPU は CUDA / ROCm / Metal。CPU でも動くが「約 100 倍遅い」。`-DGPU_RUNTIME=CPU` でビルドできる。
- 公式の Dockerfile は CUDA 用のみ。Linux arm64 向けの libtorch の zip は配布されていない。

実機で確認:

- CPU ビルド（libtorch 2.13 の Python wheel から lib と include だけ取り出して使用）で学習できる。
- 学習解像度 1416×1064・11 枚で 1 反復あたり 0.9〜1.5 秒（Apple Silicon 8 コアの Docker）。
- 進行状況を `Step 123: 0.3098 [41%]` の形で 1 反復ごとに出力する。

採用した点:

- SfM のカメラモデルを SIMPLE_RADIAL に固定（OpenSplat が読める）。
- 作業領域を「ルート直下に `images/` と `sparse/0/`」の形にした。
- 既定は `--cpu`、反復回数は控えめ（chart の既定 1000）。進行状況はログの `Step N` を読んで表示。
- AGPL のソフトをネットワーク越しに使わせる構成になるので、README にライセンス上の注意を書いた。

### SplatTransform / SOG

<https://developer.playcanvas.com/user-manual/splat-transform/> とその配下（CLI reference、Docker）、
<https://developer.playcanvas.com/user-manual/gaussian-splatting/formats/sog/>、
<https://github.com/playcanvas/splat-transform>

分かったこと:

- npm パッケージ `@playcanvas/splat-transform`（3.11.2、MIT）。Node.js 22 以上。
- `splat-transform input.ply output.sog` で 1 ファイルの SOG（中身は ZIP）になる。PLY の 1/15〜1/20 程度。
- GPU を使うのは球面調和係数の k-means だけで、**`-g cpu` を付ければ GPU 無しで動く**（5〜10 倍遅い）。
- SOG の座標系は 3DGS の PLY と同じ。PlayCanvas では Z 軸まわりに 180° 回して表示するのが定石。

実機で確認: `splat-transform -w -g cpu` が GPU の無いコンテナで動き、2.0MB の PLY が約 110KB になった（1 秒）。

採用した点: SOG 変換を独立したステップにし、失敗しても PLY で表示できるようにした。

### SuperSplat Viewer / PlayCanvas エンジン

<https://github.com/playcanvas/supersplat-viewer>、
<https://developer.playcanvas.com/user-manual/gaussian-splatting/building/your-first-app/engine/>

分かったこと:

- `@playcanvas/supersplat-viewer` は設定ファイルやポスター画像まで含む「完成品のビューア」（MIT）。
- エンジンだけでも、`new Asset(name, 'gsplat', { url })` と `entity.addComponent('gsplat', { asset })` で表示できる。
- カメラ操作は同梱の `scripts/esm/camera-controls.mjs`（`CameraControls`。`reset(focus, position)` あり）。

ソースで確認（playcanvas 2.24.1）: gsplat の読み込みは **URL の拡張子**（`ply` / `sog`）でパーサを選ぶ。

採用した点:

- 自前の画面（タブ、失敗理由の表示）に組み込みたいので、完成品のビューアではなくエンジンを直接使った
  （`web/src/viewer/SplatViewer.tsx`）。
- 成果物の URL の末尾に拡張子つきのファイル名を付けられるようにした
  （`/api/scans/{id}/assets/gaussian_sog/scene.sog`）。

### gltfpack

<https://github.com/zeux/meshoptimizer/blob/master/gltf/README.md>

分かったこと:

- 入力は `.obj` / `.gltf` / `.glb`、出力は `.gltf` / `.glb`（MIT）。**PLY は読めない**。
- 既定では頂点を量子化する（`KHR_mesh_quantization`）。`-noq` で無効。`-c` / `-cc` は meshopt 圧縮で、
  読み込み側にデコーダが要る。
- 配布バイナリ（v1.3）は Linux x86_64 / macOS arm64 / Windows。Linux arm64 は無い。

実機で確認: OBJ + MTL + PNG から、テクスチャを埋め込んだ GLB が出来て、three.js の GLTFLoader で表示できる。

採用した点: ソースからビルド（両アーキテクチャ対応のため）。`-noq` を付け、圧縮は使わない
（拡張に対応していないソフトでも読めるようにするため）。

### カメラ（MDN）

<https://developer.mozilla.org/ja/docs/Web/API/MediaDevices/getUserMedia>

分かったこと:

- 安全なコンテキスト（HTTPS か localhost）でのみ使える。それ以外では `navigator.mediaDevices` が `undefined`。
- 失敗は `NotAllowedError` / `NotFoundError` / `NotReadableError` / `OverconstrainedError` などで区別できる。
- 背面カメラは `facingMode: "environment"`。`{ exact: ... }` にすると、無い端末でエラーになる。
- `<input type="file" capture>` は対応が限定的。

採用した点:

- 呼ぶ前に安全なコンテキストかを調べ、失敗は種類ごとに日本語で説明する（`web/src/camera/cameraErrors.ts`）。
- `facingMode` は `ideal` で指定。`capture` 属性は使わず、`accept="image/*" multiple` の写真選択を用意。
- サーバーが `Permissions-Policy: camera=(self)` を返す。

### Cloudflare の制限

<https://developers.cloudflare.com/support/troubleshooting/http-status-codes/4xx-client-error/error-413/>
<https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/>

分かったこと:

- リクエストボディの上限は Free / Pro で **100MB**（超えると 413）。
- オリジンが **125 秒**以内に応答しないと 524。Enterprise 以外は変更できない。
- これらが Cloudflare Tunnel 経由の通信にも同じく適用されるかは、読んだページには明記が無かった（**未確認**）。

採用した点:

- サーバーの既定の上限を 95MB にし、ブラウザ側で長辺 2048px に縮小してから送る。
- アップロードは受け付けたらすぐ `202 Accepted` を返し、復元は非同期で行う。

### 自宅クラスタ

<https://github.com/ieyoukan/home-manifests>（リポジトリ）と、`kubectl get`（閲覧のみ）

分かったこと:

- `bootstrap/apps.yaml` が `argocd-apps/` を再帰的に読む（App of Apps）。
- 外部リポジトリの chart は `repoURL` + `path: charts/<名前>` で参照し、`valuesObject` で上書きしている。
- 秘密は `apps/<アプリ>/*.enc.yaml`（SealedSecret）に置き、`<アプリ>-secrets` という Application
  （sync-wave -1）で先に同期している。作成は `create-sealed-secret.sh`。
- Ingress は `ingressClassName: cloudflare`（Cloudflare Tunnel Ingress Controller）。
- ノードは amd64 が 4 台（ワーカーは 5 CPU / 16 GB）。**GPU の拡張リソースは無い**。
  StorageClass `proxmox-data` と IngressClass `cloudflare` が存在する。

採用した点: 同じ形の Application を 2 つ（本体と secrets）用意した。chart の既定値は GPU 無しで動く設定。

## 3. 未確認・要注意の点

- Cloudflare Tunnel 経由での実際のアップロード上限とタイムアウト。
- OpenSplat（AGPL-3.0）を使うサービスを第三者に公開する場合の義務の詳細。公開範囲を広げる前に確認が必要。
- サンプル写真（openMVG/ImageDataset_SceauxCastle）はリポジトリにライセンスの明記が無い
  （著作権表示のみ）。同梱せず、検証時に取得して使うだけにしている。
