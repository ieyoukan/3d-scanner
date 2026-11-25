# syntax=docker/dockerfile:1
#
# 3D Scanner のコンテナイメージ（CPU 専用）。役割ごとに 3 つのイメージに分ける（--target で選ぶ）:
#
#   web     画面（Vite でビルドした静的ファイル）を nginx で配る。数十 MB。画面だけ直したときはこれだけ作り直す
#   api     Go の API（受付・状態・成果物の配信）。OS の道具を含まない distroless の上に Go の単一バイナリだけ
#   worker  Go のワーカー（復元ジョブ・3DGS・位置合わせ）と、3D 復元の外部 CLI 一式（COLMAP・OpenMVS・OpenSplat 等）
#
# なぜ分けるのか: 1 つのイメージ・1 つの Pod にまとめていたころは、画面を少し直してデプロイするだけで
# 処理中の 3D 生成が止められ（Pod を入れ替えるため）、数十秒つながらなくなった。分ければ画面と API は止めずに
# 入れ替えられ、重い worker のイメージはほとんど作り直さずに済む。
#
# ステージ構成:
#   cpp-build-base   C++ ツールをビルドするための共通の開発パッケージ（Debian 13）
#   colmap-build     COLMAP（SfM・歪み補正・位置合わせ）
#   openmvs-build    OpenMVS（CPU で動く密な復元・メッシュ化・テクスチャ）
#   opensplat-build  OpenSplat（3D Gaussian Splatting の学習）
#   gltfpack-build   gltfpack（OBJ → GLB 変換）
#   node-tools       splat-transform（PLY → SOG 変換、Node.js 製）
#   worker-libs      実行に要る共有ライブラリのパッケージ名を、ビルドした実行ファイルから割り出す
#   web-build / server-build
#   web / api / worker   最終イメージ
#
# 版の選び方（2026-11 時点）:
#   worker の土台は Debian 13（trixie、現行の安定版）。CGAL 6・nanoflann・libjxl など OpenMVS v2.4.0 の依存が apt でそろう。
#     Ubuntu 26.04 LTS も試したが、Boost 1.90 では boost_system の CMake 設定が無くなっていて（1.69 からヘッダのみ）、
#     OpenMVS v2.4.0 の設定（Boost の system を要求する）が通らなかった。Ubuntu である必要は無い。
#   api は distroless（Debian 13 を土台にした、シェルもパッケージ管理も無い最小のイメージ）。Go の単一バイナリだけを置く。
#   web は nginx の非 root 版（Alpine）。
#   Node.js 26（Active LTS "Lithium"）、Go 1.27（最新）。
#   COLMAP はソースからビルドする: ディストリの COLMAP は古く（Debian 13 は 3.10、Ubuntu 26.04 でも 3.12。
#     ディストリはリリース時の版で固定する）、どのディストリの版も CUDA なしでビルドされている（COLMAP 公式の手順に記載）。
#     Homebrew は最新版（4.2.1）を追いかけるが、macOS 向けなのでコンテナには使えない。
#
# なぜ CUDA を入れないのか: 配備先のクラスタに GPU が無い。GPU を足したときは CUDA 入りの worker を別に作る。

ARG DEBIAN_VERSION=trixie
ARG NODE_VERSION=26
ARG GO_VERSION=1.27
ARG NGINX_IMAGE=nginxinc/nginx-unprivileged:stable-alpine
ARG DISTROLESS_IMAGE=gcr.io/distroless/static-debian13:nonroot

# ----------------------------------------------------------------------------
# C++ ビルド用の共通ベース
# ----------------------------------------------------------------------------
FROM debian:${DEBIAN_VERSION} AS cpp-build-base
ENV DEBIAN_FRONTEND=noninteractive
# COLMAP 公式の doc/install.rst（4.2.1）から GUI(Qt) と MKL を除いたものに、OpenMVS（nanoflann・libjxl・
# Boost の iostreams/serialization・画像形式）と OpenSplat（OpenCV、libtorch の入手に使う pip）の分を足したもの。
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git cmake ninja-build build-essential unzip pkg-config \
        libboost-program-options-dev libboost-graph-dev libboost-system-dev \
        libboost-iostreams-dev libboost-serialization-dev libboost-thread-dev \
        libeigen3-dev libopenimageio-dev openimageio-tools libmetis-dev \
        libgoogle-glog-dev libsqlite3-dev libglew-dev libcgal-dev libceres-dev \
        libsuitesparse-dev libgmp-dev libmpfr-dev \
        libnanoflann-dev libjxl-dev libpng-dev libjpeg-dev libtiff-dev zlib1g-dev \
        libopencv-dev python3-pip \
    && rm -rf /var/lib/apt/lists/* \
    # OpenImageIO の CMake 設定は OpenCV の include ディレクトリの存在を要求する（COLMAP 公式手順の回避策）。
    && mkdir -p /usr/include/opencv4

# 並列コンパイル数。COLMAP は1ファイルあたり 1〜2GB のメモリを使うことがあるので、8GB の環境では 4 程度にする。
ARG BUILD_JOBS=4

# ----------------------------------------------------------------------------
# COLMAP
# ----------------------------------------------------------------------------
FROM cpp-build-base AS colmap-build
ARG COLMAP_VERSION=4.2.1
ARG BUILD_JOBS
RUN git clone --depth 1 --branch ${COLMAP_VERSION} https://github.com/colmap/colmap.git /src/colmap
# 主なオプションの意図:
#   CUDA_ENABLED=OFF     GPU なし。
#   GUI_ENABLED=OFF      Qt 不要。
#   ONNX_ENABLED=OFF     学習ベースの特徴量（ALIKED 等）を使わないので onnxruntime を入れない。
#   LSD_ENABLED=OFF      LSD（線分検出）は AGPL。使わないので外し、COLMAP 本体を BSD のまま保つ。
#   DOWNLOAD_ENABLED=OFF 実行時にモデル等を自動ダウンロードさせない。
#   CGAL_ENABLED=ON      delaunay_mesher（簡易メッシュ）に必要。
RUN cmake -S /src/colmap -B /src/colmap/build -GNinja \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/opt/colmap \
        -DCUDA_ENABLED=OFF \
        -DGUI_ENABLED=OFF \
        -DONNX_ENABLED=OFF \
        -DLSD_ENABLED=OFF \
        -DDOWNLOAD_ENABLED=OFF \
        -DCGAL_ENABLED=ON \
        -DTESTS_ENABLED=OFF \
        -DCCACHE_ENABLED=OFF \
    && ninja -C /src/colmap/build -j ${BUILD_JOBS} install \
    && strip /opt/colmap/bin/colmap \
    && rm -rf /opt/colmap/include /opt/colmap/lib /src/colmap/build

# ----------------------------------------------------------------------------
# OpenMVS（AGPL-3.0。別の実行ファイルとして呼ぶだけなので、このアプリのコードには影響しない）
# ----------------------------------------------------------------------------
FROM cpp-build-base AS openmvs-build
ARG OPENMVS_VERSION=v2.4.0
ARG BUILD_JOBS
RUN git clone --depth 1 https://github.com/cdcseacave/VCG.git /src/vcglib \
    && git clone --depth 1 --recurse-submodules --shallow-submodules --branch ${OPENMVS_VERSION} \
        https://github.com/cdcseacave/openMVS.git /src/openMVS
# v2.4.0 は OpenCV 4.11 で追加された定数 cv::IMWRITE_JPEGXL_QUALITY（値 640）を使うが、Debian 13 の OpenCV は 4.10。
# この定数は .jxl で書き出すときだけ使う（このアプリは書き出さない）ので、値に置き換えてビルドする。
RUN grep -rl "cv::IMWRITE_JPEGXL_QUALITY" /src/openMVS | xargs -r sed -i 's/cv::IMWRITE_JPEGXL_QUALITY/640/g' \
    && cmake -S /src/openMVS -B /src/openMVS/build -GNinja \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_INSTALL_PREFIX=/opt/openmvs \
        -DVCG_ROOT=/src/vcglib \
        -DOpenMVS_USE_CUDA=OFF \
        -DOpenMVS_USE_PYTHON=OFF \
    && cmake --build /src/openMVS/build -j ${BUILD_JOBS} \
    && cmake --install /src/openMVS/build \
    && mkdir -p /opt/openmvs/run \
    # 使う 4 つだけを持ち出す（Interface 系の他の形式・ビューア・テストは使わない）。
    && for b in InterfaceCOLMAP DensifyPointCloud ReconstructMesh TextureMesh; do \
         cp /opt/openmvs/bin/OpenMVS/$b /opt/openmvs/run/$b && strip /opt/openmvs/run/$b; \
       done \
    && rm -rf /src/openMVS/build

# ----------------------------------------------------------------------------
# OpenSplat
# ----------------------------------------------------------------------------
FROM cpp-build-base AS opensplat-build
ARG OPENSPLAT_VERSION=v1.2.2
ARG TORCH_VERSION=2.13.0
ARG BUILD_JOBS
# libtorch は「CPU 版の Python wheel」の中身（lib / include / cmake 設定）だけを使う（arm64 向けの公式 zip が無いため）。
# --python-version: libtorch の .so は Python の版に依らないが、wheel は Python の版ごとに分かれている。
#   OS の Python に合う wheel が無いことがあるので、wheel が確実にある版を指定する。
RUN pip download --no-deps --only-binary=:all: --python-version 3.12 --dest /tmp/whl \
        --index-url https://download.pytorch.org/whl/cpu "torch==${TORCH_VERSION}" \
    && mkdir -p /opt/pytorch \
    && { unzip -q -o /tmp/whl/torch-*.whl 'torch/lib/*' 'torch/include/*' 'torch/share/*' 'torch.libs/*' -d /opt/pytorch \
         || [ $? -eq 11 ]; } \
    && rm -rf /tmp/whl \
    && test -f /opt/pytorch/torch/share/cmake/Torch/TorchConfig.cmake
RUN git clone --depth 1 --branch ${OPENSPLAT_VERSION} https://github.com/WebODM/OpenSplat.git /src/opensplat
RUN cmake -S /src/opensplat -B /src/opensplat/build -GNinja \
        -DCMAKE_BUILD_TYPE=Release \
        -DGPU_RUNTIME=CPU \
        -DCMAKE_PREFIX_PATH=/opt/pytorch/torch \
    && ninja -C /src/opensplat/build -j ${BUILD_JOBS} opensplat \
    && mkdir -p /opt/opensplat/bin \
    && cp /src/opensplat/build/opensplat /opt/opensplat/bin/opensplat \
    && strip /opt/opensplat/bin/opensplat \
    && rm -rf /opt/pytorch/torch/include /opt/pytorch/torch/share /src/opensplat/build

# ----------------------------------------------------------------------------
# gltfpack（meshoptimizer に同梱）
# ----------------------------------------------------------------------------
FROM cpp-build-base AS gltfpack-build
ARG MESHOPTIMIZER_VERSION=v1.3
RUN git clone --depth 1 --branch ${MESHOPTIMIZER_VERSION} https://github.com/zeux/meshoptimizer.git /src/meshoptimizer \
    && cmake -S /src/meshoptimizer -B /src/meshoptimizer/build -GNinja \
        -DCMAKE_BUILD_TYPE=Release -DMESHOPT_BUILD_GLTFPACK=ON \
    && ninja -C /src/meshoptimizer/build gltfpack \
    && mkdir -p /opt/gltfpack/bin \
    && cp /src/meshoptimizer/build/gltfpack /opt/gltfpack/bin/gltfpack \
    && strip /opt/gltfpack/bin/gltfpack

# ----------------------------------------------------------------------------
# splat-transform（Node.js 製の CLI）
# ----------------------------------------------------------------------------
FROM node:${NODE_VERSION}-trixie-slim AS node-tools
ARG SPLAT_TRANSFORM_VERSION=3.11.2
RUN npm install -g --omit=dev "@playcanvas/splat-transform@${SPLAT_TRANSFORM_VERSION}" \
    && npm cache clean --force

# ----------------------------------------------------------------------------
# 実行に要る共有ライブラリのパッケージを、ビルドした実行ファイルから割り出す
# ----------------------------------------------------------------------------
# 実行用イメージには -dev パッケージを入れない（数百 MB 増える）。かといって実行用のパッケージ名
# （libopencv-core410 など）を手で列挙すると、OS の版を上げるたびに名前が変わって壊れる。
# そこで、開発パッケージの入った環境で ldd を使い、各実行ファイルが読み込む .so がどのパッケージのものかを
# dpkg -S で引いて一覧にする。
FROM cpp-build-base AS worker-libs
COPY --from=colmap-build /opt/colmap/bin/colmap /check/colmap
COPY --from=openmvs-build /opt/openmvs/run/ /check/
COPY --from=opensplat-build /opt/opensplat/bin/opensplat /check/opensplat
COPY --from=opensplat-build /opt/pytorch /opt/pytorch
COPY --from=gltfpack-build /opt/gltfpack/bin/gltfpack /check/gltfpack
RUN set -eu; \
    export LD_LIBRARY_PATH=/opt/pytorch/torch/lib; \
    for f in /check/*; do ldd "$f"; done \
      | awk '$3 ~ /^\// {print $3}' | grep -v '^/opt/' | sort -u \
      | xargs -r -n1 sh -c 'dpkg -S "$(readlink -f "$0")" 2>/dev/null || dpkg -S "$0"' \
      | cut -d: -f1 | sort -u > /worker-packages.txt; \
    cat /worker-packages.txt

# ----------------------------------------------------------------------------
# Web（Vite + React + TypeScript）
# ----------------------------------------------------------------------------
FROM node:${NODE_VERSION}-trixie-slim AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ----------------------------------------------------------------------------
# Go（API とワーカーは同じバイナリ。ROLE で役割を選ぶ）
# ----------------------------------------------------------------------------
FROM golang:${GO_VERSION}-trixie AS server-build
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
# CGO_ENABLED=0: C ライブラリに依存しない単一バイナリにする（distroless の上でそのまま動く）。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scanner-server ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reconstruct ./cmd/reconstruct

# ============================================================================
# 最終イメージ: web
# ============================================================================
FROM ${NGINX_IMAGE} AS web
COPY deploy/docker/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=web-build /src/web/dist /usr/share/nginx/html
# 追加の設定の置き場所（空。docker compose で /api の転送設定をここに差し込む）。
USER root
RUN mkdir -p /etc/nginx/extra
USER 101
# nginx-unprivileged は UID 101 で 8080 番を待ち受ける（root で動かない）。
EXPOSE 8080

# ============================================================================
# 最終イメージ: api
# ============================================================================
FROM ${DISTROLESS_IMAGE} AS api
COPY --from=server-build /out/scanner-server /app/scanner-server
# UID は worker と同じにする（同じ PVC のファイルを読み書きするため）。
USER 10001:10001
ENV ROLE=api \
    LISTEN_ADDR=:8080 \
    DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/app/scanner-server"]

# ============================================================================
# 最終イメージ: worker
# ============================================================================
FROM debian:${DEBIAN_VERSION}-slim AS worker
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=worker-libs /worker-packages.txt /tmp/worker-packages.txt
RUN apt-get update \
    && xargs -a /tmp/worker-packages.txt apt-get install -y --no-install-recommends ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* /tmp/worker-packages.txt
COPY --from=colmap-build /opt/colmap/bin/colmap /usr/local/bin/colmap
COPY --from=openmvs-build /opt/openmvs/run/ /usr/local/bin/OpenMVS/
COPY --from=opensplat-build /opt/opensplat/bin/opensplat /usr/local/bin/opensplat
COPY --from=opensplat-build /opt/pytorch /opt/pytorch
COPY --from=gltfpack-build /opt/gltfpack/bin/gltfpack /usr/local/bin/gltfpack
COPY --from=node-tools /usr/local/bin/node /usr/local/bin/node
COPY --from=node-tools /usr/local/lib/node_modules/@playcanvas/splat-transform /usr/local/lib/node_modules/@playcanvas/splat-transform
# - libtorch の場所を動的リンカに教える（LD_LIBRARY_PATH に頼らない）。
# - splat-transform の実行ファイル名を package.json の bin から解決して PATH に置く。
# - 共有ライブラリが1つでも見つからなければビルド失敗にする（入れ忘れを実行時ではなくビルド時に見つける）。
RUN set -eu; \
    { echo /opt/pytorch/torch/lib; [ -d /opt/pytorch/torch.libs ] && echo /opt/pytorch/torch.libs; } > /etc/ld.so.conf.d/pytorch.conf; \
    ldconfig; \
    bin_rel="$(node -e 'const b=require("/usr/local/lib/node_modules/@playcanvas/splat-transform/package.json").bin; console.log(typeof b==="string"?b:b["splat-transform"])')"; \
    ln -s "/usr/local/lib/node_modules/@playcanvas/splat-transform/${bin_rel}" /usr/local/bin/splat-transform; \
    chmod +x "/usr/local/lib/node_modules/@playcanvas/splat-transform/${bin_rel}"; \
    for b in /usr/local/bin/colmap /usr/local/bin/opensplat /usr/local/bin/gltfpack /usr/local/bin/node /usr/local/bin/OpenMVS/*; do \
        if ldd "$b" | grep -q "not found"; then \
            echo "共有ライブラリが不足しています: $b"; ldd "$b" | grep "not found"; exit 1; \
        fi; \
    done; \
    colmap help >/dev/null; opensplat --version; splat-transform --version; \
    /usr/local/bin/OpenMVS/DensifyPointCloud --help >/dev/null 2>&1 || true
RUN useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin scanner \
    && mkdir -p /data && chown -R scanner:scanner /data
COPY --from=server-build /out/scanner-server /out/reconstruct /app/
ENV ROLE=worker \
    LISTEN_ADDR=:8081 \
    DATA_DIR=/data \
    OPENMVS_DIR=/usr/local/bin/OpenMVS \
    # splat-transform が設定やキャッシュを書こうとしても読み取り専用のルートFSで困らないようにする。
    HOME=/tmp
USER 10001:10001
WORKDIR /app
EXPOSE 8081
VOLUME ["/data"]
# tini を PID 1 にする: Go が起動した COLMAP 等の子プロセスが孤児になっても回収（reap）され、SIGTERM も転送される。
ENTRYPOINT ["/usr/bin/tini", "--", "/app/scanner-server"]
