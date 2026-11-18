# 3D Scanner — MVP 設計書（実装反映版）

> 初版: 2026-11-16（実装前の設計） / 改訂: 2026-11-16（実装と実機検証の結果を反映）
>
> 対象: アプリ本体 `ieyoukan/3d-scanner` と、GitOps リポジトリ `ieyoukan/home-manifests`
>
> 目的: **スマホのブラウザで撮影 → Go サーバーへ送信 → SfM → メッシュ／3DGS → ブラウザで 3D 閲覧**。学習しやすさを最優先する。

この文書は「いまのコードがどうなっているか」を説明する。実装前の設計から変えた点は
[§11 初版からの変更点](#11-初版からの変更点) にまとめた。

## 1. 目標と範囲

### MVP で実装したもの

- Vite + React + TypeScript のスマホ向け Web アプリ（撮影・写真選択・確認・送信・処理状況・3D 閲覧）。
- Go の HTTP API と非同期ジョブ（1 プロセスに API・ワーカー・掃除係）。
- COLMAP による SfM、メッシュ化、テクスチャ生成。gltfpack による GLB 化。
- OpenSplat による 3D Gaussian Splatting。SplatTransform による SOG 化。
- PostgreSQL でジョブの状態を管理し、写真と成果物は PVC に保存。保存期限で自動削除。
- アクセス制限はアプリに持たせず、前段の Cloudflare Access に任せる（同一オリジン確認だけ行う）。
- ギャラリー（完成したものを保存）と、続きを撮る（VPS による位置合わせと、撮り足した写真での作り直し）。
- 3DGS の後回し（メッシュで完成にし、3DGS は別の待ち行列で学習。新しいスキャンに順番を譲る）。
- 撮影の補助: 自動撮影、静止画での撮影、写真の焦点距離（EXIF）を位置推定の初期値に使う。
- Dockerfile、docker compose、Helm chart、GitHub Actions。

### MVP で実装していないもの

- アカウント、共同編集。
- AR 表示（WebXR / ARCore）、撮影中のリアルタイム追跡（Visual SLAM）。位置合わせは写真 1 枚ずつ、サーバーで行う。
- 自動撮影のガイド、深度センサー、3DGS の編集、Unity / VRChat への直接公開。
- 分散ジョブキュー、複数ノードでの並列処理、分割アップロード。

「履歴を保存しない」と「データを保存しない」は別。ジョブの状態は DB に、写真と成果物は PVC に置く。
完成したもの（succeeded / partial）はギャラリーに保存し（`scans.saved`）、利用者が削除するまで残す。
このとき中間ファイル（`dense/`）だけは消す。失敗・中止したものは保存期間（既定 24 時間）を過ぎたら削除する。

## 2. 技術スタック

| 役割 | 採用技術 | 版（検証時） |
| --- | --- | --- |
| フロント | Vite + React + TypeScript | Vite 8 / React 19 / TypeScript 7 |
| スタイル | Tailwind CSS v4（設定ファイル無し、`@theme` で色を定義） | 4.3 |
| 撮影 | `getUserMedia`、だめなら `<input type="file" multiple>` | – |
| メッシュ表示 | three.js（GLTFLoader / OrbitControls / PLYLoader） | 0.187 |
| 3DGS 表示 | PlayCanvas エンジンの gsplat コンポーネント＋同梱の CameraControls | 2.24 |
| API / ジョブ | Go 標準ライブラリ（`net/http`、`database/sql`）＋ pgx | Go 1.27 |
| DB | PostgreSQL | 17 |
| SfM / メッシュ / テクスチャ | COLMAP（CPU 専用ビルド） | 4.2.1 |
| 3DGS | OpenSplat（CPU 専用ビルド、libtorch 2.13） | 1.2.2 |
| メッシュ変換 | gltfpack（meshoptimizer） | 1.3 |
| Splat 変換 | `@playcanvas/splat-transform` | 3.11.2 |
| 配備 | Docker、Helm、Argo CD | – |
| 永続化 | Proxmox CSI（StorageClass `proxmox-data`） | – |

### 環境の制約（実機で確認したこと）

- **配備先のクラスタに GPU は無い。** `kubectl get nodes` で GPU の拡張リソースが 0 であることを確認した
  （amd64 の 4 ノード。ワーカーは 5 CPU / 16 GB）。
- **COLMAP の密な復元（`patch_match_stereo`）は CUDA（または AMD HIP）が必須。** 公式 FAQ にも
  「CUDA 対応 GPU が無ければ dense 再構成以外のすべてが使える」とある。
- **OpenSplat の GPU 実行は CUDA / ROCm / Metal のみ。** Intel の内蔵 GPU は対象外。
- そのため既定のイメージは CPU 専用で、メッシュは「疎な点群からの簡易メッシュ」、
  3DGS は CPU 学習になる（[§6](#6-復元パイプライン)）。

## 3. アーキテクチャ

```text
[スマホ / PC のブラウザ]
  React: 撮影・写真の確認・送信・処理状況・3D 表示
        |
        v HTTPS（Cloudflare Tunnel。1 リクエスト 100MB まで）
[Go サーバー]  1 Pod・1 プロセス・replicas=1
  ├─ HTTP API   受付、状態の問い合わせ、成果物と Web の配信
  ├─ ワーカー   待ち行列から 1 件ずつ取り出して外部 CLI を実行
  └─ 掃除係     保存期限の過ぎたスキャンを削除
        |                      |
        v                      v
  [PostgreSQL]           [/data: PVC（ReadWriteOnce）]
   ジョブの状態            写真・中間ファイル・成果物・ログ
                               |
                               v
                         COLMAP SfM
                   カメラの位置と向き ＋ 疎な点群
                        /              \
                       v                v
              メッシュ分岐            3DGS 分岐
        (GPU あり) MVS → Poisson      OpenSplat の学習
        (GPU なし) Delaunay（疎）          |
               mesh_texturer              v
               PLY → OBJ（Go）         splat.ply
               gltfpack                splat-transform
                       |                  |
                       v                  v
                   mesh.glb           scene.sog
```

API とワーカーを同じ Pod・同じプロセスにしているのは、作業領域の PVC が `ReadWriteOnce`
（同時に 1 ノードからしかマウントできない）ため。分けると「同じノードに載る」保証が必要になる。
ジョブは 1 件ずつ実行する（SfM も 3DGS も全コアとメモリを使うため）。

## 4. 画面

| URL | 内容 |
| --- | --- |
| `/` | 撮影（カメラ映像・シャッター・写真追加）と確認（サムネイル・個別削除・ぼけの指摘・送信） |
| `/scans/:id` | 処理状況。4 段階（SfM / MVS・メッシュ / 3DGS / 完成）の状態・説明・所要時間・ログ |
| `/scans/:id/view` | 3D ビューア。メッシュ / 3DGS / 点群のタブ、視点リセット、ダウンロード |
| `/gallery` | 保存した 3D データの一覧 |
| `/scans/:id/resume` | 続きを撮る。撮影画面に位置合わせ（VPS）の状況と小さな地図を重ね、確認画面に大きな地図を出す |

設計上の決まり:

- **カメラは必須にしない。** HTTP 接続、権限の拒否、カメラ無しのどれでも、理由を説明して
  写真選択に切り替える。
- **進捗率を作らない。** 表示するのは、サーバーが返した状態と実測の所要時間だけ。
  例外は 3DGS の学習で、OpenSplat 自身が出力する反復回数（`Step N`）を読んで表示する。
- **失敗を隠さない。** 出来なかった出力のタブは消さず、理由つきで無効にして残す。
- **向きを揃える。** SfM の座標系は向きも原点も任意なので、サーバーが `scene.json`
  （上方向・中心・カメラ位置）を作り、3 つのビューアが同じ回転と平行移動を使う。

配色は暗い無彩色に琥珀色の強調 1 色。写真と 3D モデルを主役にするため。

## 5. データモデルと API

### テーブル（`server/internal/store/migrations/0001_init.sql`）

| テーブル | 役割 |
| --- | --- |
| `scans` | スキャン 1 件。状態、期限、着手回数、エラー、撮影側の付加情報（JSONB） |
| `scan_frames` | 写真 1 枚ごと。寸法、ハッシュ、撮影時刻、カメラ情報（JSONB） |
| `scan_steps` | ステップ（sfm / mvs / mesh / splat / sog）ごとの状態と説明 |
| `scan_assets` | 検証を通った成果物の目録（種類、相対パス、サイズ） |

後から足した列: `scans.saved`・`title`（`0002_gallery.sql`、ギャラリー）、`scans.parent_id`・`base_frame_count`
（`0003_resume.sql`、続きの版が前の版を指す。前の版を消すと NULL になり、続きの版は残る）。
後から足したテーブル: `splat_jobs`（`0004_splat_jobs.sql`、あとで作る 3DGS の待ち行列。待っている間と学習中だけ行がある）。

`scan_frames.captured_at` と `camera`（JSONB）、`scans.capture_meta`（JSONB）は MVP では
記録するだけだが、VPS や ARCore を足すときに列を増やさず項目を追加できるようにしてある。

### 状態

スキャン: `queued → running →（partial | succeeded | failed）`、途中で `cancelled`、期限で `expired`。

全体の状態は、ステップと成果物から次の規則で決める（`scan.Summarize`）:

1. 見られる成果物（メッシュか 3DGS）が 1 つも無ければ `failed`。
2. 失敗したステップが無ければ `succeeded`（`skipped` は失敗に数えない）。
3. それ以外は `partial`。

### API

| メソッドとパス | 内容 |
| --- | --- |
| `GET /healthz` | 生存確認（依存先を見ない） |
| `GET /readyz` | DB と保存先を確認 |
| `GET /api/config` | 上限値、サーバーの構成 |
| `GET /api/scans` | ギャラリーの一覧（新しい順、期限切れを除く）。`?limit=`（最大 60）と `?after=`（前回の `nextCursor`）で続きを読む |
| `POST /api/scans` | `multipart/form-data`（`images` 複数＋任意の `metadata`）。`202 Accepted` |
| `GET /api/scans/{id}` | 状態、ステップ、成果物、待ち順 |
| `GET /api/scans/{id}/logs?step=sfm` | ステップの処理ログ（末尾 256KB） |
| `GET /api/scans/{id}/assets/{kind}` | 成果物。末尾に `/scene.sog` のようなファイル名を付けてもよい |
| `GET /api/scans/{id}/thumbnail` | ギャラリー用の縮小画像（受付時に1枚目の写真から作る JPEG、長辺 480） |
| `POST /api/scans/{id}/localize` | 位置合わせ（VPS）。項目 `image` に写真 1 枚。地図の座標系での位置・向き・一致した 3D 点の数を返す（同期。制限 80 秒） |
| `POST /api/scans/{id}/splat` | 3DGS を（作り直しを含めて）待ち行列に入れる。`202`。既に入っていれば `409` |
| `POST /api/scans/{id}/resume` | 続きを撮る。`POST /api/scans` と同じ形（1 枚から）。前の版を引き継いだ新しい版を登録して `202` |
| `PATCH /api/scans/{id}` | 名前の変更（`{"title": "..."}`、80 文字まで） |
| `DELETE /api/scans/{id}` | 処理中ならキャンセル、終了済みなら削除 |

成果物の種類: `sparse_ply` / `scene_json` / `mesh_glb` / `gaussian_ply` / `gaussian_sog`。

### 作業領域（PVC）

```text
/data/scans/<uuid>/        ← COLMAP / OpenSplat の「プロジェクトルート」
  images/                  入力の写真（frame_0001.jpg ...。名前はサーバーが付ける）
  database.db              COLMAP の内部 DB（SQLite。PostgreSQL とは別物）
  sparse/0/                SfM の結果
  sparse_txt/              SfM の結果のテキスト版（要約の計算用）
  dense/                   歪み補正済みの画像とカメラ、メッシュ、テクスチャ
  output/                  配信する成果物
  logs/                    ステップごとの外部 CLI の出力
  thumbnail.jpg            ギャラリー用の縮小画像
  base/                    続きの版だけ。前の版から引き継いだ地図（完成したら消す）
    database.db            前の版の COLMAP DB（ハードリンク）
    model/                 前の版の sparse/0（ハードリンク）
    frames.txt             引き継いだ写真の名前
/data/tmp/                 位置合わせの一時作業（終わったら消す。起動時にも空にする）
```

続きの版は受付時に、前の版の `images/*`・`database.db`・`sparse/0/*`・`thumbnail.jpg` を
ハードリンクで引き継ぐ（容量も時間もほぼ使わない。前の版を消しても実体は残る）。
COLMAP が書き換える `database.db` だけは、ワーカーが処理の始めに `base/` から複製する。

### 入力の検証と安全性

- スキャン ID はサーバーが乱数から作る UUID。URL から受け取った ID は形式を検査してから使う。
- 写真の種類は先頭バイトで判定（JPEG / PNG のみ）。枚数、1 枚のサイズ、全体のサイズ、
  画素数、短辺の上限・下限を検証。完全に同じ写真は除く。
- DB には相対パスだけを保存し、配信の直前に「作業領域の外を指していないか」
  （`..`、絶対パス、外を指すシンボリックリンク）を確かめる。
- 外部 CLI はシェルを通さず、引数を分けて実行する。
- 受付の前に、待ち行列の件数とディスクの空きを確認する。

### アクセス制限

- アプリ自体にログインは無い（以前は共有トークン方式だったが、Cloudflare Access を前段に置く方針に変えて取り除いた）。
  公開する URL には必ず Access などの認証を置く。
- 状態を変える POST / DELETE は `Origin` を検査する。前段の認証 Cookie は別サイトのページが出したリクエストにも
  付き得るので、同じドメインの別サブドメインからの CSRF を断つために残している。
- アップロードの同時実行数、待ち行列の件数、ディスクの空きの確認で、使われすぎを防ぐ。

## 6. 復元パイプライン

実装は `server/internal/reconstruction/pipeline.go`。各ステップは外部 CLI が 0 で終わっても、
出力ファイルの存在・大きさ・先頭バイトを確かめてから成功とする。

| ステップ | コマンド | 出力 |
| --- | --- | --- |
| `sfm` | `colmap feature_extractor` → `exhaustive_matcher` → `mapper` → `model_converter` | `sparse/0/`、`output/sparse.ply`、`output/scene.json` |
| `mvs`（GPU あり） | `colmap image_undistorter` → `patch_match_stereo` → `stereo_fusion` | `dense/fused.ply` |
| `mesh`（GPU あり） | `colmap poisson_mesher` → `mesh_texturer` → PLY→OBJ（Go）→ `gltfpack` | `output/mesh.glb` |
| `mesh`（GPU なし） | `colmap image_undistorter` → `delaunay_mesher --input_type sparse` → `mesh_texturer` → PLY→OBJ（Go）→ `gltfpack` | `output/mesh.glb` |
| `splat` | `opensplat <root> -n N -d D -o output/splat.ply [--cpu]` | `output/splat.ply` |
| `sog` | `splat-transform -w -g cpu splat.ply scene.sog` | `output/scene.sog` |

- SfM の結果を 2 つの分岐が共有する。分岐どうしは独立で、片方が失敗してももう片方は実行する。
- `mesh_texturer` の出力は OBJ ではなく「面ごとの UV を持つ PLY ＋ texture.png」。
  gltfpack は PLY を読めないので、Go で OBJ + MTL に書き直す（`reconstruction/obj.go`）。
- メッシュの作り方は `MESH_MODE`（`auto` / `dense` / `sparse`）で決まる。`auto` は
  `colmap version` の表示（`with CUDA` / `without GPU support`）で判定する。

### 3DGS の後回し

`SPLAT_DEFERRED=true`（既定）のとき、ワーカーは `sfm` → `mvs` → `mesh` までで止まり、メッシュが出来ていれば
スキャンを完成（`succeeded` / `partial`、保存）にして、3DGS を `splat_jobs` に入れる。`splat` / `sog` のステップは
`pending` のまま「あとで作ります」と表示する。メッシュが出来なかったときは、見られるものを作るためその場で 3DGS も学習する。

3DGS のワーカー（`jobs/splat.go`）は別の goroutine で、待ち行列から1件ずつ学習する。

- `SPLAT_YIELD=true`（CPU 向けの既定）: 位置推定・メッシュの待ち（queued / running）がある間は始めない。
  学習中にワーカーが新しいスキャンを取り出したら、学習を止めて待ち行列の元の位置に戻す（着手回数に数えない）。
  OpenSplat は途中から再開できないので、後で最初からやり直す。
- `SPLAT_YIELD=false`（GPU 向け）: 新しいスキャンと並行して学習する。
- 学習結果は別名（`splat.new.ply` / `scene.new.sog`）に書き、検証を通ってから置き換える。作り直しに失敗しても前の 3DGS は残る。
  成果物は作り直すと同じ URL のまま中身が変わるので、`Cache-Control: private, no-cache`（変わっていなければ 304）にした。
- 再起動で中断された学習は、起動時に待ち行列へ戻す。3 回中断されたら 3DGS を失敗にし、スキャンを `partial` にする。
- 削除・期限切れのときは、学習を止めて終わるのを待ってからファイルを消す。

### 焦点距離の手掛かり

撮影側は送る前に写真の EXIF から 35mm 換算の焦点距離（`FocalLengthIn35mmFilm`）を読み、メタデータで送る
（ブラウザで縮小し直すと EXIF は消える）。サーバーは写真がすべて同じ寸法で、半分以上に値があり、それらが ±5% に
収まるときだけ `camera_hint.json` に残し、`feature_extractor --ImageReader.camera_params f,cx,cy,0` で
SIMPLE_RADIAL の初期値として渡す（`f = f35 × 対角線[px] ÷ 43.27`）。COLMAP はこれを事前値（Prior）として扱う。

続きの版（`base/` がある）の `sfm` は、前の版の結果から始める:

| 手順 | 続きの版での違い |
| --- | --- |
| `feature_extractor` | DB に既にある写真は COLMAP が飛ばす（`IMAGE_EXISTS`）。今回の写真だけを抽出 |
| `exhaustive_matcher` | マッチ済みの組は飛ばす（全組マッチ済みの DB で再実行すると約 1.7 秒で終わることを確認） |
| `mapper` | `--input_path base/model --output_path sparse/0 --Mapper.fix_existing_frames 1`。前の版のカメラ位置を固定し、今回の写真を 1 枚ずつ登録 |
| 判定 | 今回の写真が 1 枚も入らなければ失敗（前の版と同じものを作り直すだけになるため） |

### 位置合わせ（VPS）

実装は `server/internal/vps`。地図は SfM の結果そのもので、作り直さない。

1. 地図の `sparse/0` をテキストに変換し、写真の一覧とカメラ（寸法）を読む。
2. 地図の `database.db` を一時ディレクトリに複製し、写真を `vps_query.jpg` として置く。
3. `feature_extractor`。写真の寸法が地図のカメラと同じなら `--ImageReader.existing_camera_id` で焦点距離を使い回す。
4. `matches_importer --match_type pairs`。地図の写真から最大 60 枚を等間隔に選んで突き合わせる。
5. `image_registrator --input_path sparse/0`。2D-3D の対応からカメラの位置と向きを解き、1 枚だけ追加する。
6. 結果をテキストに変換し、写真の位置（`C = -Rᵀt`）と向き（R の 3 行目）、3D 点に対応づいた特徴点の数を返す。

同時に 1 件だけ実行する（2 件目は `429`）。登録されなかったときは `localized: false`（エラーではない）。
位置と向きは `scene.json` と同じ座標系なので、画面は `up` と `center` を使って真上から見た地図に描く
（`web/src/resume/mapView.ts`）。

学習用の解説は [docs/learning/](learning/) にある。

## 7. 配備

```text
ieyoukan/3d-scanner
  web/                    Vite + React
  server/                 Go
  deploy/docker/          Dockerfile
  charts/3d-scanner/      Helm chart
  docker-compose.yml      手元で全体を動かす構成
  docs/                   設計書・調査記録・学習ノート

ieyoukan/home-manifests
  argocd-apps/3d-scanner.yaml           Argo CD Application（上の chart を GitHub から参照）
  argocd-apps/3d-scanner-secrets.yaml   SealedSecret を先に同期する Application
  apps/3d-scanner/                      SealedSecret の置き場
```

Helm chart の要点（詳細は `charts/3d-scanner/values.yaml` のコメント）:

- アプリは Deployment、`replicas: 1` 固定、更新方式は `Recreate`。
- 作業領域の PVC と、PostgreSQL（StatefulSet、1 レプリカ）の PVC。どちらも `ReadWriteOnce`。
  `postgresql.enabled=false` で既存の DB に切り替えられる。
- 秘密の値は `existingSecret` で受け取る。未設定なら `helm template` の時点で止まる。
- 非 root（UID 10001）、読み取り専用のルートファイルシステム、権限の昇格なし。
- 変えられる値: イメージ名とタグ、Ingress のホスト、StorageClass、PVC の容量、DB の接続先、
  GPU リソース、保存期間、各種の上限とパイプラインの設定。

## 8. テストと検証

何を確認し、何が未検証かは [README の「検証の記録」](../README.md#検証の記録) にまとめてある。

## 9. この先の見通し

実装済み: 保存した SfM の特徴点と新しい写真の対応づけ（PnP + RANSAC による Relocalization）、
追加撮影の座標合わせとモデルの更新、前回の撮影位置と今回の位置の可視化。

- 撮影中の連続した追跡。いまは写真 1 枚ごとにサーバーで 10〜20 秒かかる。端末側の追跡（WebXR / ARCore）で
  位置合わせの間をつなぐ。
- AR 表示: 位置合わせの結果を基準に、3D データや目印を現実の映像に重ねる。
- 撮影時の端末姿勢（ARCore / VIO）の記録。`scan_frames.camera` に入れる想定。
- 次に撮るべき位置の提示（地図の上で、写真が少ない方向を示す）。
- 地図が大きくなったときの突き合わせ相手の選び方（vocabulary tree による画像検索）。

## 10. 参考 URL

調査した URL と、そこから採用した点は [docs/research.md](research.md) に記録した。

## 11. 初版からの変更点

| 項目 | 初版の案 | 実装 | 理由 |
| --- | --- | --- | --- |
| Helm chart の場所 | `home-manifests/apps/3d-scanner/` | アプリのリポジトリの `charts/3d-scanner/` | 実装指示（アプリに chart を置き、home-manifests からは GitHub 経由で参照）に従った。既存の他アプリとも同じ形 |
| Pod の構成 | `web` と `api` を別の Deployment | 1 つの Deployment（Go が Web も配る） | 同一オリジンになり CSRF 対策と CSP が単純になる。Pod が 1 つで済む |
| メッシュ（GPU なし） | 記載なし（MVS 前提） | `delaunay_mesher --input_type sparse` による簡易メッシュ | クラスタに GPU が無く、`patch_match_stereo` を実行できないため。粗いが CPU で数秒 |
| `mesh_texturer` の出力 | OBJ + MTL + テクスチャを想定 | 面ごとの UV を持つ PLY + PNG | COLMAP 4.2.1 のソースで確認。Go で OBJ に変換してから gltfpack に渡す |
| 作業領域の構成 | `colmap/database.db`、`colmap/sparse/` | 直下に `database.db`、`sparse/0/` | OpenSplat がプロジェクトルート直下の `images/` と `sparse/0/` を読むため |
| ステップ名 | `sfm, mvs, mesh, splat, conversion` | `sfm, mvs, mesh, splat, sog` | 変換は 2 種類（GLB 化は mesh に含め、SOG 化を独立させた）。SOG だけ失敗しても PLY で見られることを表せる |
| 成果物 | `mesh_glb, gaussian_ply, gaussian_sog, sparse_ply` | 左に加えて `scene_json` | ビューアの向きと初期視点を決めるため |
| アップロード | 分割アップロードの検討 | 1 リクエスト＋ブラウザ側での縮小 | 長辺 2048px に縮めれば 80 枚でも 100MB に収まる。分割は必要になってから |
| 公開 URL | `scanner.youkan.uk` は候補 | Application の values で指定 | DNS とトンネルの経路は Ingress Controller が作る。実際の公開はマージ後 |
| 保存期間 | すべて 24 時間で削除（長期保存は範囲外） | 完成したものはギャラリーに保存して残す（`KEEP_COMPLETED`、既定 true） | 利用者の要望。撮った 3D データを手元に残したい。失敗・中止したものは従来どおり期限で消す |
| 3DGS の作成時期 | メッシュと続けて作る | 既定はあとで作る（`SPLAT_DEFERRED`）。新しいスキャンに順番を譲る（`SPLAT_YIELD`） | GPU が無く CPU で数十分かかるため。メッシュを先に見られるようにし、GPU を入れたら作り直せるようにした |
| VPS | 次のフェーズ | 写真 1 枚の位置合わせと、続きを撮る（前の版を引き継いだ新しい版） | 利用者の要望。保存した 3D データに後から撮り足したい。前の版を書き換えないので、追加がうまくいかなくても元に戻す必要がない |
