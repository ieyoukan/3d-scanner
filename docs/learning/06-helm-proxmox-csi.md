# 06. Helm と Proxmox CSI — クラスタに載せるときの制約

## このノートで分かること

- このアプリを Kubernetes に載せるために、何を・なぜそう設定しているか。
- PVC と `ReadWriteOnce` の意味。なぜレプリカを 1 に固定しているのか。
- 秘密の値（DB パスワード）を Git に平文で置かない方法。
- GPU の扱い。なぜ既定では使わないのか。
- Argo CD（GitOps）でどう配備されるか。

## 目的

手元の `docker compose` で動いたものを、自宅クラスタ（k3s、Proxmox 上の VM）で動かす。
そのために必要なものを **Helm chart**（`charts/3d-scanner/`）にまとめ、設定の違いは values で吸収する。

## 入力

| 入力 | どこから |
| --- | --- |
| コンテナイメージ | `ghcr.io/ieyoukan/3d-scanner`（GitHub Actions が公開） |
| values | `charts/3d-scanner/values.yaml` の既定値＋ Argo CD の Application での上書き |
| 秘密の値 | クラスタ上の Secret（SealedSecret から作られる） |
| ストレージ | StorageClass `proxmox-data`（Proxmox CSI） |

### 配備先の実態（2026-11-16 に `kubectl get` で確認）

- ノードは amd64 が 4 台。コントロールプレーン 3 台（2 CPU / 8 GB）と、ワーカー 1 台（5 CPU / 16 GB）。
- **GPU の拡張リソースは無い**（`nvidia.com/gpu` も `gpu.intel.com/*` も 0）。
- StorageClass は `local-path`（既定）、`proxmox-data`、`proxmox-monaka`。
- IngressClass は `cloudflare`（Cloudflare Tunnel Ingress Controller）と `traefik`。

## 処理

### chart が作るもの

```text
Deployment  scanner-web          画面（nginx）。RollingUpdate
Deployment  scanner-api          API（Go、ROLE=api）。RollingUpdate。PVC を使う
Deployment  scanner-worker       ワーカー（Go、ROLE=worker ＋ COLMAP・OpenMVS 等）。replicas: 1、Recreate。PVC を使う
Service     scanner-web / scanner-api   ポート 80 → コンテナの 8080
Service     scanner-worker       ポート 8081（クラスタの中だけ。api からの頼みごと）
Ingress     scanner              外部公開（任意）。/api → scanner-api、それ以外 → scanner-web
ConfigMap   scanner              秘密でない設定（環境変数）
PVC         scanner-data         作業領域 /data
StatefulSet scanner-postgresql   PostgreSQL（任意。既存の DB に切り替え可）
Service     scanner-postgresql
PVC         data-scanner-postgresql-0   StatefulSet が自動で作る
```

リソース名が `3d-scanner` ではなく `scanner` なのは、Service の名前が英字で始まる必要があるため
（DNS ラベルの規則。数字で始められない）。

### PVC と ReadWriteOnce

Pod のファイルシステムは、Pod が消えると一緒に消える。写真や成果物を残すには、
Pod の外にあるストレージ（PersistentVolume）をマウントする。その「このくらいの容量が欲しい」という
要求が PVC（PersistentVolumeClaim）。

```yaml
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: proxmox-data
  resources:
    requests:
      storage: 30Gi
```

Proxmox CSI は、この要求を受けて Proxmox 上に**仮想ディスク**を作り、Pod が載るノード（VM）に接続する。
仮想ディスクは、物理的なディスクと同じで、同時に 1 台の VM にしか接続できない。
これが `ReadWriteOnce`（同時に 1 つのノードからだけ読み書きできる）の正体。

```text
          ノード A                     ノード B
   ┌──────────────────┐        ┌──────────────────┐
   │ Pod: scanner     │        │                  │
   │   /data ─────────┼──┐     │  （接続できない）  │
   └──────────────────┘  │     └──────────────────┘
                         v
              [仮想ディスク 30Gi]  ← 1 台の VM にしか繋がらない
```

StorageClass の `volumeBindingMode: WaitForFirstConsumer` は「Pod の配置先が決まってからディスクを作る」設定。
先にディスクを作ると、Pod が別のノードに配置されたときに接続できなくなるため。

### なぜワーカーのレプリカを 1 に固定するのか

`templates/worker.yaml` では `replicas: 1` を直接書いていて、values では変えられない。理由は 2 つ。

1. **ストレージ**: 2 つ目の Pod が別のノードに載ると、同じディスクをマウントできずに起動しない。
2. **ジョブの回復**: ワーカーは起動時に「`running` のまま残ったジョブ」を、前回の自分の残骸として
   やり直す（[05](05-go-jobs.md)）。Pod が複数あると、他の Pod が実行中のジョブまで巻き戻してしまう。

同じ理由で、更新方式は `Recreate`（古い Pod を止めてから新しい Pod を作る）。
既定の `RollingUpdate` は新旧を一時的に同時に動かすので、上の前提が崩れる。
更新中は数十秒つながらなくなるが、MVP では整合性を優先した。

処理能力を増やしたくなったら、レプリカを増やすのではなく、構成を変える必要がある
（成果物をオブジェクトストレージに置く、回復の仕組みを「自分が取ったジョブだけ」に直す、など）。

### 役割を分けた理由と、同じノードに載せる仕組み

以前は 1 つの Deployment（Web + API + ワーカーが 1 プロセス、Recreate）だった。そのため画面を少し直してデプロイする
だけで処理中の 3D 生成が止まり、数十秒つながらなくなった。今は web・api・worker の 3 つに分け、CI は中身の変わった
イメージだけを公開するので、画面や API の更新では worker は入れ替わらない。web と api は RollingUpdate
（新しい Pod が準備できてから古い Pod を止める）なので、更新中も止まらない。

api と worker は同じ PVC を使う。ReadWriteOnce なので、2 つは同じノードに居なければならない。両方の Pod に
`3d-scanner/data-volume: scanner-data` のラベルを付け、`requiredDuringSchedulingIgnoredDuringExecution` の podAffinity で
「このラベルの Pod が居るノードに載る」ようにしている。最初の 1 つは相手が居なくても載せられる
（自分自身がラベルに一致する場合の Kubernetes の決まり）。api の RollingUpdate で一時的に 2 つになっても、
同じノードなので同じディスクをマウントできる。

### PostgreSQL は StatefulSet

データを持つソフトには StatefulSet を使う。Deployment との違い:

- Pod に決まった名前（`scanner-postgresql-0`）が付く。
- `volumeClaimTemplates` から、その Pod 専用の PVC が自動で作られる。Pod が作り直されても同じ PVC に再接続される。
- StatefulSet を消しても PVC は残る（データを誤って失わない）。

`PGDATA` をボリュームの直下ではなく `pgdata/` にしているのは、ext4 のボリュームには最初から
`lost+found` があり、PostgreSQL が「空でない場所」での初期化を拒むため。

既存の PostgreSQL を使う場合は `postgresql.enabled=false` にして `externalDatabase.*` を指定する。

### 秘密の値

DB のパスワードは、Git に平文で置いてはいけない。chart は 2 通りの渡し方を用意している。

| 方法 | 用途 |
| --- | --- |
| `postgresql.auth.existingSecret` | 本番。別の手段で作った Secret の名前を指定する |
| `postgresql.auth.password` | 手元での検証。`--set` で渡す（values ファイルには書かない） |

どちらも指定しないと、`helm template` の時点でエラーになって止まる（`templates/_helpers.tpl` の `3d-scanner.validate`）。
「秘密が未設定のまま配備され、起動してから気づく」ことを防ぐ。

chart の中で乱数からパスワードを作る方法は採っていない。Argo CD は同期のたびに `helm template` を実行するので、
そのたびに違う値が生成され、DB のパスワードが勝手に変わってしまう。

自宅クラスタでは **SealedSecret** を使う。公開鍵で暗号化した Secret を Git に置き、
クラスタ内のコントローラが秘密鍵で復号して本物の Secret を作る。暗号化されているので Git に置ける。

```bash
# home-manifests のルートで（kubeseal と、k3s-1 への SSH が必要）
cat > 3d-scanner-secrets.env <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 24)
EOF
./create-sealed-secret.sh --name 3d-scanner-secrets --namespace 3d-scanner \
  3d-scanner-secrets.env --output-dir apps/3d-scanner
rm 3d-scanner-secrets.env     # 平文は残さない（*.env は .gitignore 済み）
```

アプリ自体にはログイン機能が無い。公開する URL には Cloudflare Access などの認証を前段に置く（README「公開するときの注意」）。

### コンテナの権限

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 10001          # イメージ内の scanner ユーザー
  fsGroup: 10001            # マウントしたボリュームをこの GID で書けるようにする
  fsGroupChangePolicy: OnRootMismatch
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities: { drop: [ALL] }
```

- root で動かさない。アップロードされたファイルを外部ツールに読ませる構成なので、
  万一ツールに穴があっても被害を小さくする。
- `fsGroup` が無いと、root 所有で作られた PVC に非 root のコンテナが書き込めない。
- ルートファイルシステムは読み取り専用。書けるのは `/data`（PVC）と `/tmp`（emptyDir）だけ。

### ヘルスチェック

| 種類 | パス | 役割 |
| --- | --- | --- |
| startupProbe | `/healthz` | 起動が終わるまで待つ（DB の準備待ちを含めて最大 150 秒） |
| livenessProbe | `/healthz` | プロセスが生きているか。失敗が続くと再起動される |
| readinessProbe | `/readyz` | DB と作業領域が使えるか。失敗中は Service の転送先から外れる |

`/healthz` は DB を見ない。DB の一時的な障害でアプリまで再起動されると、実行中の復元ジョブが
無駄に中断されるため。

### CPU とメモリ

```yaml
resources:
  requests: { cpu: "1", memory: 2Gi }
  limits:   { cpu: "4", memory: 8Gi }
config:
  pipeline:
    numThreads: 4     # limits.cpu と同じ値にする
```

外部 CLI は既定で「ノードの全コア」ぶんのスレッドを立てる。コンテナに CPU 制限があると、
制限より多いスレッドが少ない CPU 時間を奪い合って、かえって遅くなる。だからスレッド数を制限に合わせる。

メモリの上限を超えると、コンテナは強制終了される（OOM Kill）。そのとき実行中だったジョブは、
再起動後にやり直され、2 回目も落ちれば `failed` になる。

重い処理なので、余力のあるノード（このクラスタではワーカー）に寄せたい場合は `nodeSelector` を使う。

### GPU

```yaml
gpu:
  enabled: false
  resourceName: nvidia.com/gpu
  count: 1
```

`enabled: true` にすると、コンテナの `limits` に GPU が追加される。ただし**既定のイメージでは使われない**。

- COLMAP の密な復元は NVIDIA CUDA（または AMD HIP）専用。
- OpenSplat の GPU 実行は CUDA / ROCm / Metal のみ。
- Intel の内蔵 GPU（`gpu.intel.com/i915`）には、どちらも対応していない。

つまり「内蔵 GPU 程度」の環境では、GPU を割り当てても速くならない。この値は、
将来 NVIDIA の GPU を足したとき（CUDA 対応のイメージに差し替えたうえで）のために用意してある。
手順のメモは [03-mvs.md](03-mvs.md)。

### Ingress と Cloudflare

```yaml
ingress:
  enabled: true
  className: cloudflare
  hosts:
    - host: scanner.youkan.uk
      paths: [{ path: /, pathType: Prefix }]
```

Cloudflare Tunnel Ingress Controller が、Ingress を見てトンネルの経路と DNS レコードを作る。
TLS は Cloudflare 側で終端するので、`tls` は空でよい。

Cloudflare（Free / Pro）の制約: アップロードは 1 回 100MB まで、応答待ちは 125 秒まで。
そのため `config.limits.maxUploadMB` は 95、アップロードは即 `202` を返す設計にしてある。

### Argo CD（GitOps）

自宅クラスタは「Git に書いたものが、クラスタの状態になる」方式。`home-manifests` リポジトリの
`argocd-apps/` に Application を置くと、Argo CD が自動で同期する（App of Apps）。

```text
home-manifests/
  bootstrap/apps.yaml                 argocd-apps/ を再帰的に読む親 Application
  argocd-apps/3d-scanner-secrets.yaml sync-wave -1。apps/3d-scanner/ の SealedSecret を先に同期
  argocd-apps/3d-scanner.yaml         sync-wave 0。このリポジトリの charts/3d-scanner を参照
  apps/3d-scanner/*.enc.yaml          SealedSecret
```

Application は chart を GitHub から直接参照する:

```yaml
source:
  repoURL: https://github.com/ieyoukan/3d-scanner.git
  targetRevision: main
  path: charts/3d-scanner
  helm:
    valuesObject:
      auth: { existingSecret: 3d-scanner-secrets }
      ...
```

**main にマージすることが、そのまま配備になる。** だから home-manifests への変更は Pull Request にして、
差分を確認してからマージする。

## 出力

配備が成功すると:

```bash
kubectl --context k3s-1 -n 3d-scanner get pods,pvc,ingress
# pod/scanner-xxxx                1/1 Running
# pod/scanner-postgresql-0        1/1 Running
# pvc/scanner-data                Bound  30Gi  RWO  proxmox-data
# pvc/data-scanner-postgresql-0   Bound  2Gi   RWO  proxmox-data
# ingress/scanner                 cloudflare  scanner.youkan.uk
```

（これは期待される出力の例。実際のクラスタには配備していない。）

## よくある失敗

| 症状 | 原因 | 確認・対処 |
| --- | --- | --- |
| Pod が `Pending` のまま | PVC が `Bound` にならない、CPU・メモリの要求を満たすノードが無い | `kubectl describe pod` の Events を見る |
| `CreateContainerConfigError` | 参照している Secret が無い | SealedSecret が同期されているか、名前とキーが合っているか |
| `Multi-Attach error` | 古い Pod がディスクを掴んだまま、新しい Pod が別ノードで起動しようとした | 更新方式が `Recreate` になっているか。古い Pod が消えるのを待つ |
| 起動するが書き込めない（permission denied） | `fsGroup` が無い、UID が違う | `podSecurityContext` を確認 |
| `CrashLoopBackOff` で DB に接続できない | Secret のキー名が違う、パスワードが違う | `postgresql.auth.existingSecretKey` を確認 |
| `ImagePullBackOff` | イメージが非公開、タグが無い | GHCR のパッケージの公開設定、`image.tag` を確認 |
| アップロードで 413 | Cloudflare の 100MB 制限 | 写真を減らす。`maxUploadMB` を 95 以下に |
| ジョブの途中で Pod が再起動する | メモリ上限を超えた（OOM Kill） | `kubectl describe pod` の `Last State: OOMKilled`。上限を上げるか、写真・解像度を減らす |

## 確認方法

クラスタに触らずに出来ること（すべて実行して確認済み）:

```bash
helm lint charts/3d-scanner --set auth.existingSecret=x --set postgresql.auth.existingSecret=x

# 自宅クラスタと同じ形で出力してみる
helm template scanner charts/3d-scanner -n 3d-scanner \
  --set auth.existingSecret=3d-scanner-secrets \
  --set postgresql.auth.existingSecret=3d-scanner-secrets \
  --set persistence.storageClass=proxmox-data \
  --set postgresql.persistence.storageClass=proxmox-data \
  --set ingress.enabled=true --set ingress.className=cloudflare \
  --set 'ingress.hosts[0].host=scanner.youkan.uk' > /tmp/rendered.yaml

# Kubernetes のスキーマに合っているか（クラスタ不要）
docker run --rm -i ghcr.io/yannh/kubeconform:v0.8.0 -strict -summary \
  -kubernetes-version 1.33.0 - < /tmp/rendered.yaml

# 秘密の値が未設定なら止まること
helm template scanner charts/3d-scanner     # → 「PostgreSQL のパスワードが未設定です」で失敗するのが正しい
```

使い捨てのクラスタで確認したこと:

本番と同じ版の k3s（v1.33.6）を Docker のコンテナ 1 つで起動し、chart を `helm install` した。

- Pod は 2 つとも `1/1 Running`、再起動 0 回。3 種類のプローブが通る。
- PVC は 2 つとも `Bound`（StorageClass は k3s 同梱の `local-path`）。
- コンテナは UID 10001、`/data` に書き込める、ルート FS への書き込みは拒否される。
- 実写真 11 枚の復元ジョブが成功し、成果物が PVC に残る。
- ジョブ実行中に Pod を削除すると、新しい Pod が同じ PVC で起動し、ジョブを最初からやり直して完了する。

この確認で分からないこと: Proxmox CSI のボリュームの挙動（ノードをまたぐ再接続、`fsGroup` の適用）、
Cloudflare の Ingress、SealedSecret の復号。これらは本番クラスタでしか確かめられない。

本番クラスタで確認すること（**未検証**。マージ後に行う）:

```bash
# 必ず最初に、対象のクラスタを確認する。kubeconfig には複数のクラスタが入っている
kubectl config current-context        # k3s-1 であること

kubectl --context k3s-1 -n 3d-scanner get pods,pvc,ingress
kubectl --context k3s-1 -n 3d-scanner logs deploy/scanner | head     # 「復元パイプラインの構成」の行
kubectl --context k3s-1 -n 3d-scanner exec deploy/scanner -- df -h /data   # PVC がマウントされているか
```

## 次の処理

配備後は、スマホから `https://scanner.youkan.uk/` を開いて、撮影から閲覧までを通す
（[01](01-capture.md)）。

## 次に読む

- [01-capture.md](01-capture.md) — 最初に戻って、実機で撮影する
- [05-go-jobs.md](05-go-jobs.md) — レプリカを 1 に固定している、もう 1 つの理由
