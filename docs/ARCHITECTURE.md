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
- Dockerfile、docker compose、Helm chart、GitHub Actions。

### MVP で実装していないもの

- アカウント、長期のスキャン履歴、共同編集、再訪問スキャン。
- VPS / Relocalization / ARCore / Visual SLAM（次のフェーズ）。
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
```

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

## 9. MVP 後（VPS フェーズ）の見通し

- ユーザーと空間 ID、位置情報（スキャンの長期保存はギャラリーとして実装済み。地図として使う
  `database.db` と `sparse/0` も残している）。
- 撮影時の端末姿勢（ARCore / VIO）の記録。`scan_frames.camera` に入れる想定。
- 保存した SfM の特徴点と新しい画像の対応づけ、PnP + RANSAC による Relocalization。
- 追加撮影の座標合わせ、モデルの更新。
- 撮影済み範囲の可視化、次に撮るべき位置の提示。

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
