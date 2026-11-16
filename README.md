# 3D Scanner

スマホのブラウザで写真を撮り、サーバーで 3D モデルを作って、ブラウザで見るための Web アプリ（MVP）。

```text
撮影（ブラウザ） → アップロード → SfM → メッシュ（GLB）
                                      └→ 3D Gaussian Splatting（PLY / SOG）
                                 → 進行状況 → 3D ビューア → ダウンロード
```

研究や学習で「まず動かして、育てていく」ための土台として作ってある。コードのコメントと文書は日本語で、
**なぜそうしているか**を書くようにしている。

- 設計の全体像: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- 調べたことと採用した点: [docs/research.md](docs/research.md)
- 学習ノート: [docs/learning/](docs/learning/)（撮影 → SfM → MVS → 3DGS → Go のジョブ → Helm の順）

## 何が出来るか

| 画面 | 内容 |
| --- | --- |
| `/` | カメラで撮影、または写真を選択。サムネイルで確認して不要な写真を削除し、送信する |
| `/scans/<id>` | 処理の進行状況。段階ごとの状態・説明・所要時間・処理ログ。中止と削除 |
| `/scans/<id>/view` | メッシュ / 3DGS / 点群をタブで切り替えて 3D 表示。GLB / PLY / SOG のダウンロード |

出来ないこと（MVP の範囲外）: アカウント、スキャンの長期保存、再訪問スキャン、VPS、3DGS の編集。
写真と結果は保存期間（既定 24 時間）を過ぎると自動で削除される。

### GPU が無い環境での動作

既定のコンテナイメージは **CPU 専用**。配備先に NVIDIA の GPU が無いことを前提にしている。

| 出力 | GPU なし（既定） | NVIDIA GPU あり（別イメージが必要。未検証） |
| --- | --- | --- |
| メッシュ | 疎な点群からの**簡易メッシュ**（粗い。数秒） | 密な復元（MVS）によるメッシュ |
| 3DGS | CPU で学習（1 反復あたり約 1 秒。遅い） | GPU で学習 |

COLMAP の密な復元（`patch_match_stereo`）は CUDA が必須で、CPU では実行できない。
この場合、画面には「実行しなかった」ことと理由が表示される。

## 必要なもの

| 用途 | 必要なもの |
| --- | --- |
| 全体をコンテナで動かす | Docker（Compose v2）。ディスクの空き 10GB 以上、メモリ 8GB 以上 |
| サーバーの開発 | Go 1.27 以上 |
| Web の開発 | Node.js 22 以上 |
| 配備 | Helm 3 以上、Kubernetes |

COLMAP などの外部ツールはコンテナイメージに入っているので、手元に入れる必要は無い。

## いちばん簡単な動かし方（Docker Compose）

```bash
./scripts/dev-env.sh          # .env を作る（アクセストークンと DB パスワードを乱数で生成）
docker compose up --build     # 初回は COLMAP と OpenSplat のビルドに 40〜60 分かかる
```

`http://localhost:8080` を開き、`.env` の `ACCESS_TOKEN` を入力する。
8080 番を他のプログラムが使っている場合は、`.env` に `APP_PORT=18080` のように書く。

サンプルの写真で試す:

```bash
./scripts/fetch-sample.sh     # samples/sceaux/images/ に 11 枚を取得（リポジトリには含めていない）
```

画面の「追加」から、取得した写真を選ぶ。

> スマホ実機のカメラで試すには **HTTPS** が必要（ブラウザは HTTP のページにカメラを使わせない）。
> `http://localhost` は例外的に許可されるが、スマホから PC の IP アドレスへ HTTP で繋ぐ場合は許可されない。
> Cloudflare Tunnel などで HTTPS の URL を用意する。HTTP のままでも「写真を選ぶ」は使える。

## 開発

### サーバー（Go）

```bash
cd server
go vet ./...
go test ./...                 # PostgreSQL を使うテストは省略される

# PostgreSQL を使う契約テストも流す場合（テストは scans テーブルを空にするので専用の DB を使う）
TEST_DATABASE_URL='postgres://user:pass@localhost:5432/scanner_test?sslmode=disable' go test ./internal/store/
```

画面だけを開発したいとき（COLMAP を入れていない PC）は、`REQUIRE_TOOLS=false` を付けると起動できる。
この場合、復元ジョブは SfM で失敗する。

```bash
DATABASE_URL='postgres://...' ALLOW_NO_AUTH=true COOKIE_SECURE=false \
REQUIRE_TOOLS=false DATA_DIR=./data go run ./cmd/server
```

### Web（Vite + React）

```bash
cd web
npm ci
npm run dev        # http://localhost:5173 。/api は http://localhost:8080 に中継される
npm run typecheck
npm test
npm run build      # dist/ に出力
```

中継先は `web/.env` の `VITE_API_TARGET` で変えられる。

### パイプラインだけを実行する

DB も HTTP も無しで、サーバーと同じ復元処理を 1 回だけ実行できる。外部 CLI をどの順で、
どんな引数で呼んでいるかを確かめるのに使う。

```bash
./scripts/fetch-sample.sh
docker run --rm -v "$PWD/samples/sceaux:/work" -e SPLAT_ITERATIONS=300 \
  --entrypoint /app/reconstruct 3d-scanner:dev -dir /work
# 結果: samples/sceaux/output/  ログ: samples/sceaux/logs/
```

## 設定（環境変数）

| 変数 | 既定値 | 内容 |
| --- | --- | --- |
| `DATABASE_URL` | – | PostgreSQL の接続文字列。または `DB_HOST` `DB_PORT` `DB_USER` `DB_PASSWORD` `DB_NAME` を個別に指定 |
| `ACCESS_TOKEN` | – | 共有のアクセストークン（16 文字以上）。未設定だと起動しない |
| `ALLOW_NO_AUTH` | `false` | `true` でトークン無しの起動を許す（閉じた環境での検証用） |
| `COOKIE_SECURE` | `true` | セッション Cookie を HTTPS でのみ送る。HTTP で試すときだけ `false` |
| `TRUST_PROXY_HEADERS` | `false` | 接続元の判定に `CF-Connecting-IP` を使う（Cloudflare 経由でしか届かない構成のみ） |
| `DATA_DIR` | `/data` | 作業領域（PVC のマウント先） |
| `WEB_DIR` | – | ビルド済み Web の場所。空なら API のみ |
| `RETENTION` | `24h` | 保存期間 |
| `MIN_IMAGES` / `MAX_IMAGES` | `8` / `80` | 写真の枚数 |
| `MAX_IMAGE_MB` / `MAX_UPLOAD_MB` | `15` / `95` | 1 枚と全体のサイズ上限（Cloudflare の 100MB 制限より小さくする） |
| `MAX_QUEUED_SCANS` | `5` | 順番待ち＋実行中の上限 |
| `MIN_FREE_DISK_MB` | `2048` | 空きがこれを下回ったら受付を断る |
| `ENABLE_MESH` / `ENABLE_SPLAT` | `true` | 各分岐を実行するか |
| `MESH_MODE` | `auto` | `auto` / `dense` / `sparse` |
| `COLMAP_MAX_IMAGE_SIZE` | `1600` | 特徴抽出・歪み補正時の長辺の上限 |
| `NUM_THREADS` | `0` | 外部 CLI のスレッド数（0 は全コア）。CPU 制限と同じ値にする |
| `SPLAT_ITERATIONS` | `1000` | 3DGS の学習反復回数（CPU では 1 反復あたり約 1 秒） |
| `SPLAT_DOWNSCALE` | `2` | 学習時の画像の縮小率 |
| `SPLAT_FORCE_CPU` | `true` | 3DGS を CPU で学習する |
| `SFM_TIMEOUT` ほか | `1h` ほか | 各ステップの制限時間（`MVS_` `MESH_` `SPLAT_` `SOG_`） |
| `REQUIRE_TOOLS` | `true` | 起動時に COLMAP を確認できなければ起動を中止する |

全項目と説明は `server/internal/config/config.go` にある。

## API の例

```bash
BASE=http://localhost:8080
TOKEN=...   # .env の ACCESS_TOKEN

# 写真を送って復元ジョブを登録する（すぐに 202 が返る）
curl -X POST "$BASE/api/scans" -H "Authorization: Bearer $TOKEN" \
  -F images=@photo1.jpg -F images=@photo2.jpg -F images=@photo3.jpg  # 実際は 8 枚以上
# → {"id":"38b5d76c-...","status":"queued","frameCount":11,"duplicatesSkipped":0,"expiresAt":"..."}

# 状態を確認する（終わるまで数秒おきに繰り返す）
curl "$BASE/api/scans/$ID" -H "Authorization: Bearer $TOKEN"
# → {"status":"running","steps":[{"name":"sfm","status":"succeeded","message":"11枚中11枚の..."}, ...],"assets":[...]}

# 処理ログ
curl "$BASE/api/scans/$ID/logs?step=sfm" -H "Authorization: Bearer $TOKEN"

# 成果物（mesh_glb / gaussian_sog / gaussian_ply / sparse_ply / scene_json）
curl -o mesh.glb "$BASE/api/scans/$ID/assets/mesh_glb" -H "Authorization: Bearer $TOKEN"

# 中止（処理中）または削除（終了済み）
curl -X DELETE "$BASE/api/scans/$ID" -H "Authorization: Bearer $TOKEN"
```

エラーは `{"error":{"code":"too_few_images","message":"写真が足りません…"}}` の形で返る。

## 写真の撮り方

1. **対象のまわりを、少しずつ移動しながら撮る。** 同じ場所で向きだけ変えても奥行きは分からない。
2. **隣の写真と 7 割ほど重ねる。** 写真どうしで同じ点を見つけて位置を合わせるので、重なりが要る。
3. **高さを変えて、もう 1 周。** 上からと下からの写真があると、隠れた面まで復元できる。
4. **最低 8 枚、できれば 20〜60 枚。**
5. **光る面・透明な物・無地の壁は苦手。** 見る角度で見た目が変わる面や、模様の無い面では同じ点を見つけられない。
6. **止まってから撮る。** ぶれた写真は位置合わせに使えない。確認画面で「ぼけ?」と出た写真は削除する。

詳しくは [docs/learning/01-capture.md](docs/learning/01-capture.md)。

## コンテナイメージ

```bash
docker build -f deploy/docker/Dockerfile -t 3d-scanner:dev .
```

COLMAP 4.2.1、OpenSplat 1.2.2、gltfpack 1.3、splat-transform 3.11.2 を CPU 専用でビルドし、
Go のサーバーとビルド済みの Web を同梱する（約 1.5GB）。linux/amd64 と linux/arm64 でビルドできる。
main に push すると、GitHub Actions が `ghcr.io/ieyoukan/3d-scanner` に公開する
（タグは `latest` と `sha-<コミット>`）。

## Kubernetes への配備（Helm）

```bash
# 構文と出力の確認
helm lint charts/3d-scanner --set auth.existingSecret=x --set postgresql.auth.existingSecret=x
helm template scanner charts/3d-scanner --set auth.existingSecret=x --set postgresql.auth.existingSecret=x

# 配備の例（秘密の値は先に Secret として作っておく）
kubectl create namespace 3d-scanner
kubectl -n 3d-scanner create secret generic 3d-scanner-secrets \
  --from-literal=ACCESS_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 24)"

helm install scanner charts/3d-scanner -n 3d-scanner \
  --set auth.existingSecret=3d-scanner-secrets \
  --set postgresql.auth.existingSecret=3d-scanner-secrets \
  --set persistence.storageClass=proxmox-data \
  --set postgresql.persistence.storageClass=proxmox-data \
  --set ingress.enabled=true --set ingress.className=cloudflare \
  --set 'ingress.hosts[0].host=scanner.example.com'
```

> `kubectl` / `helm` を実行する前に、`kubectl config current-context` で対象のクラスタを必ず確認すること。

主な値（全項目は `charts/3d-scanner/values.yaml`）:

| 値 | 内容 |
| --- | --- |
| `image.repository` / `image.tag` | イメージ名とタグ |
| `ingress.hosts[].host` / `ingress.className` | 公開するホスト名と IngressClass |
| `persistence.storageClass` / `persistence.size` | 作業領域の StorageClass と容量 |
| `postgresql.enabled` / `externalDatabase.*` | 内蔵の PostgreSQL を使うか、既存の DB に繋ぐか |
| `auth.existingSecret` / `postgresql.auth.existingSecret` | 秘密の値を入れた Secret の名前 |
| `config.retention` | 保存期間 |
| `config.pipeline.*` | メッシュの作り方、3DGS の反復回数など |
| `gpu.enabled` / `gpu.resourceName` | GPU の割り当て（既定のイメージでは使われない） |
| `resources` | CPU とメモリの要求・上限 |

自宅クラスタ（Argo CD）への登録は `ieyoukan/home-manifests` の `argocd-apps/3d-scanner.yaml` で行う。
仕組みは [docs/learning/06-helm-proxmox-csi.md](docs/learning/06-helm-proxmox-csi.md)。

### 公開するときの注意

- アクセストークンは「知っている人なら誰でも使える」共有の鍵。利用者ごとの管理は無い。
  インターネットに公開する場合は、Cloudflare Access などの認証を前段に重ねることを勧める。
- 写真には部屋の中や人が写りうる。保存期間を必要以上に長くしない。
- Cloudflare（Free / Pro）はアップロードを 1 回 100MB で打ち切る。`config.limits.maxUploadMB` は 95 以下にする。

## 検証の記録

2026-11-16 時点。実行していないことは「未検証」と書いている。

### 確認できたこと

| 項目 | 方法 | 結果 |
| --- | --- | --- |
| Go | `go vet ./...`、`go test -race ./...` | 成功（9 パッケージ） |
| PostgreSQL 実装 | 契約テストを PostgreSQL 17 に対して実行 | 成功 |
| Web | `tsc --noEmit`、`vitest run`、`vite build` | 成功（単体テスト 69 件） |
| Helm | `helm lint`、`helm template`（4 通りの構成）、kubeconform（Kubernetes 1.33、strict） | 成功 |
| コンテナイメージ | linux/arm64 でビルド | 成功（1.45GB） |
| SfM（実画像） | サンプル 11 枚を COLMAP 4.2.1 で実行 | 11/11 枚の位置を推定、3D 点 8,348 個、再投影誤差 0.63px、約 1 分 |
| メッシュ（簡易） | `delaunay_mesher`（疎）→ `mesh_texturer` → GLB | 頂点 4,586・面 9,166、3.7MB、約 2 秒 |
| 3DGS（CPU） | OpenSplat 300 / 700 反復 | 7 分 23 秒 / 10 分 11 秒。PLY 2.0MB |
| SOG | `splat-transform -g cpu` | 2.0MB → 約 110KB、1 秒 |
| サーバー経由の一連の流れ | Docker Compose で起動し、ブラウザ（スマホ幅）で操作 | ログイン → 写真選択 → 確認・削除 → 送信 → 処理状況 → 3 つのタブで 3D 表示 → 保存 |
| 部分成功 | 3D 点が極端に少ない入力 | メッシュ失敗・3DGS 成功 → `partial`。失敗理由を表示し、タブは無効で残る |
| SfM の失敗 | 模様の無い単色画像 | `failed`。後段は「実行せず」、成果物なし |
| キャンセル | SfM 実行中に DELETE | `cancelled`。外部プロセスが止まり、作業領域が削除される |
| 再起動からの回復 | 実行中にコンテナを再起動 | 起動時に待ち行列へ戻り、2 回目の着手で完了 |
| 非 root・読み取り専用 | Compose の `read_only: true`、UID 10001 | 動作 |
| Helm chart の実インストール | 使い捨ての k3s v1.33.6（Docker 上、StorageClass は `local-path`）に `helm install` | Pod 2 つが Running、プローブ通過、PVC 2 つが Bound。UID 10001・読み取り専用ルート FS のまま復元ジョブが成功 |
| Kubernetes 上での回復 | 同じ k3s で、ジョブ実行中に Pod を削除 | 新しい Pod が同じ PVC で起動し、中断ジョブを 2 回目の着手で完了。以前のスキャンも保持 |
| カメラ経路 | `getUserMedia` を合成映像に差し替えて撮影 | 映像表示・撮影・取り込みが動作。拒否時は写真選択に切り替わる |

### 未検証のこと

| 項目 | 理由 | 確認の手順 |
| --- | --- | --- |
| 密な復元（MVS）によるメッシュ | CUDA 対応の GPU が無い。単体テストでコマンドの順番と引数だけ確認 | CUDA 入りのイメージを作り、`MESH_MODE=dense` で実行する（[docs/learning/03-mvs.md](docs/learning/03-mvs.md)） |
| GPU での 3DGS 学習 | 同上 | CUDA 版の OpenSplat を入れ、`SPLAT_FORCE_CPU=false` |
| スマホ実機（Android Chrome）での撮影 | 開発環境から実機に繋いでいない | HTTPS の URL を用意し、実機で撮影から閲覧まで通す |
| linux/amd64 のイメージ | 手元は arm64。amd64 は GitHub Actions でビルドする | Actions の Image ワークフローの結果を見る |
| 本番クラスタへの配備 | 実際のクラスタには適用していない。使い捨ての k3s では確認したが、Proxmox CSI のボリューム、Cloudflare の Ingress、SealedSecret の復号は本番でしか確かめられない | home-manifests の Pull Request をマージし、Pod・PVC・Ingress を確認する |
| Cloudflare Tunnel 経由のアップロード | 未配備 | 80 枚程度を実機から送り、413 / 524 が出ないことを確認する |
| 20 枚を超える写真での処理時間とメモリ | サンプルは 11 枚 | 枚数を増やして、所要時間とメモリ使用量を測る |

## 使用している OSS とライセンス

| ソフトウェア | 用途 | ライセンス |
| --- | --- | --- |
| [COLMAP](https://github.com/colmap/colmap) 4.2.1 | SfM、メッシュ化、テクスチャ | BSD-3-Clause（LSD は無効にしてビルド） |
| [OpenSplat](https://github.com/WebODM/OpenSplat) 1.2.2 | 3D Gaussian Splatting の学習 | **AGPL-3.0** |
| [LibTorch](https://pytorch.org/) 2.13 | OpenSplat が使う数値計算 | BSD-3-Clause |
| [meshoptimizer / gltfpack](https://github.com/zeux/meshoptimizer) 1.3 | OBJ → GLB | MIT |
| [SplatTransform](https://github.com/playcanvas/splat-transform) 3.11 | PLY → SOG | MIT |
| [PlayCanvas Engine](https://github.com/playcanvas/engine) | 3DGS の表示 | MIT |
| [three.js](https://github.com/mrdoob/three.js) | メッシュと点群の表示 | MIT |
| [React](https://react.dev/)、[React Router](https://reactrouter.com/) | 画面 | MIT |
| [Vite](https://vite.dev/)、[Vitest](https://vitest.dev/) | ビルド、テスト | MIT |
| [Tailwind CSS](https://tailwindcss.com/) | スタイル | MIT |
| [pgx](https://github.com/jackc/pgx) | PostgreSQL ドライバ | MIT |
| [PostgreSQL](https://www.postgresql.org/) | DB | PostgreSQL License |
| [tini](https://github.com/krallin/tini) | コンテナの PID 1 | MIT |

**OpenSplat（AGPL-3.0）について**: このアプリは OpenSplat を別プロセスとして実行するだけで、コードは取り込んでいない。
ただし AGPL には「ネットワーク越しに利用させる場合もソースを提供する」という条項があるので、
OpenSplat を改変して第三者に公開する場合は、その改変版のソースを提供する必要がある。
公開範囲を広げる前に、ライセンスの原文を確認すること。

このリポジトリ自体のライセンスは未設定（決めたら `LICENSE` を追加する）。

サンプル写真（`scripts/fetch-sample.sh` が取得するもの）は openMVG の
[ImageDataset_SceauxCastle](https://github.com/openMVG/ImageDataset_SceauxCastle)
（Copyright 2012 Pierre MOULON）。ライセンスの明記が無いため、このリポジトリには含めていない。
