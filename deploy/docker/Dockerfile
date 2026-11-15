# syntax=docker/dockerfile:1
#
# 3D Scanner のコンテナイメージ（CPU専用）。
#
# このイメージ1つに「Go の API/ワーカー」「ビルド済みの Web」「3D復元の外部CLI一式」を入れる。
# MVP では API とワーカーが同じ Pod で同じ PVC を使うため（docs/learning/06 を参照）、
# イメージも1つにまとめた方が配備が単純になる。
#
# ステージ構成（上から順に依存する）:
#   cpp-build-base   C++ツールをビルドするための共通の開発パッケージ
#   colmap-build     COLMAP（SfM / MVS / メッシュ / テクスチャ）
#   opensplat-build  OpenSplat（3D Gaussian Splatting の学習）
#   gltfpack-build   gltfpack（OBJ → GLB 変換）
#   node-tools       splat-transform（PLY → SOG 変換、Node.js 製）
#   tools            上記の実行ファイルだけを集めた実行用ベース
#   web-build        Vite で Web をビルド
#   server-build     Go で API/ワーカーをビルド
#   runtime          最終イメージ
#
# なぜ COLMAP をソースからビルドするのか:
#   Ubuntu 24.04 の apt 版は 3.9.1 と古く、mesh_texturer（4.0.0 で追加）が無く、
#   CPU指定のオプション名も現行ドキュメントと違う。学習用に「公式ドキュメントどおり」
#   動かしたいので、リリース版 4.x をビルドする。
#
# なぜ CUDA を入れないのか:
#   配備先のクラスタには GPU が無い（2026-11-16 に kubectl で確認）。CUDA 入りイメージは
#   数GB大きくなるうえ使われない。GPU を足したときは CUDA_ENABLED を ON にした
#   別イメージを作る（docs/learning/03-mvs.md に手順のメモあり）。

ARG UBUNTU_VERSION=24.04
ARG NODE_VERSION=22
ARG GO_VERSION=1.27

# ----------------------------------------------------------------------------
# C++ ビルド用の共通ベース
# ----------------------------------------------------------------------------
FROM ubuntu:${UBUNTU_VERSION} AS cpp-build-base
ENV DEBIAN_FRONTEND=noninteractive

# パッケージ一覧は COLMAP 公式の doc/install.rst（4.2.1）から、GUI(Qt) と MKL を除いたもの。
#   - Qt を除く理由: サーバーでは GUI を使わない（-DGUI_ENABLED=OFF）。
#   - MKL を除く理由: x86 専用で巨大。線形代数は Ceres が使う OpenBLAS/LAPACK で足りる。
# libopencv-dev と python3-pip は OpenSplat 用（pip は libtorch の入手にだけ使う）。
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git cmake ninja-build build-essential unzip \
        libboost-program-options-dev libboost-graph-dev libboost-system-dev \
        libeigen3-dev libopenimageio-dev openimageio-tools libmetis-dev \
        libgoogle-glog-dev libsqlite3-dev libglew-dev libcgal-dev libceres-dev \
        libsuitesparse-dev \
        libopencv-dev python3-pip \
    && rm -rf /var/lib/apt/lists/* \
    # Ubuntu の OpenImageIO の CMake 設定は OpenCV の include ディレクトリの存在を要求する
    # （COLMAP 公式手順にある回避策。libopencv-dev を入れていれば既に存在する）。
    && mkdir -p /usr/include/opencv4

# 並列コンパイル数。COLMAP は1ファイルあたり 1〜2GB のメモリを使うことがあるので、
# メモリ 8GB の環境では 4 程度に抑える（多すぎるとコンパイラが OOM で落ちる）。
ARG BUILD_JOBS=4

# ----------------------------------------------------------------------------
# COLMAP
# ----------------------------------------------------------------------------
FROM cpp-build-base AS colmap-build
ARG COLMAP_VERSION=4.2.1
ARG BUILD_JOBS
RUN git clone --depth 1 --branch ${COLMAP_VERSION} https://github.com/colmap/colmap.git /src/colmap
# 主なオプションの意図:
#   CUDA_ENABLED=OFF     GPU なし。patch_match_stereo（dense MVS）は実行時にエラーになる。
#   GUI_ENABLED=OFF      Qt 不要。
#   ONNX_ENABLED=OFF     学習ベースの特徴量（ALIKED 等）を使わないので onnxruntime を入れない。
#   LSD_ENABLED=OFF      LSD（線分検出）は AGPL。使わないので外し、COLMAP 本体を BSD のまま保つ。
#   DOWNLOAD_ENABLED=OFF 実行時にモデル等を自動ダウンロードさせない（サーバーの動作を固定する）。
#   CGAL_ENABLED=ON      delaunay_mesher に必要。
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
    # 実行ファイル以外（ヘッダや静的ライブラリ）は実行時に不要なので持ち出さない。
    && rm -rf /opt/colmap/include /opt/colmap/lib /src/colmap/build

# ----------------------------------------------------------------------------
# OpenSplat
# ----------------------------------------------------------------------------
FROM cpp-build-base AS opensplat-build
ARG OPENSPLAT_VERSION=v1.2.2
ARG TORCH_VERSION=2.13.0
ARG BUILD_JOBS
# libtorch（PyTorch の C++ ライブラリ）の入手方法:
#   公式の libtorch zip は linux/arm64 向けが無いので、amd64/arm64 の両方で同じ手順になるよう
#   「CPU 版の Python wheel」をダウンロードして中身（lib / include / cmake 設定）だけ使う。
#   wheel は zip なので unzip で展開できる。Python のコードは一切実行しない。
RUN pip download --no-deps --only-binary=:all: --dest /tmp/whl \
        --index-url https://download.pytorch.org/whl/cpu "torch==${TORCH_VERSION}" \
    && mkdir -p /opt/pytorch \
    # torch.libs/（同梱の libgomp 等）は wheel によっては存在しない。unzip は「一致しない
    # パターンがあった」とき終了コード 11 を返すので、それだけは成功扱いにする。
    && { unzip -q -o /tmp/whl/torch-*.whl 'torch/lib/*' 'torch/include/*' 'torch/share/*' 'torch.libs/*' -d /opt/pytorch \
         || [ $? -eq 11 ]; } \
    && rm -rf /tmp/whl \
    && test -f /opt/pytorch/torch/share/cmake/Torch/TorchConfig.cmake
RUN git clone --depth 1 --branch ${OPENSPLAT_VERSION} https://github.com/WebODM/OpenSplat.git /src/opensplat
# GPU_RUNTIME=CPU: CUDA/ROCm/Metal を使わず CPU のみでビルドする（OpenSplat の CMake が用意している値）。
RUN cmake -S /src/opensplat -B /src/opensplat/build -GNinja \
        -DCMAKE_BUILD_TYPE=Release \
        -DGPU_RUNTIME=CPU \
        -DCMAKE_PREFIX_PATH=/opt/pytorch/torch \
    && ninja -C /src/opensplat/build -j ${BUILD_JOBS} opensplat \
    && mkdir -p /opt/opensplat/bin \
    && cp /src/opensplat/build/opensplat /opt/opensplat/bin/opensplat \
    && strip /opt/opensplat/bin/opensplat \
    # 実行時に要るのは共有ライブラリだけ。ヘッダと CMake 設定は捨てる。
    && rm -rf /opt/pytorch/torch/include /opt/pytorch/torch/share /src/opensplat/build

# ----------------------------------------------------------------------------
# gltfpack（meshoptimizer に同梱）
# ----------------------------------------------------------------------------
FROM cpp-build-base AS gltfpack-build
ARG MESHOPTIMIZER_VERSION=v1.3
# 公式の配布バイナリ（gltfpack-ubuntu.zip）は x86_64 のみなので、arm64 でも動くようソースから作る。
# テクスチャ圧縮（BasisU）は組み込まない: テクスチャは元の PNG/JPEG のまま GLB に埋め込めば足りる。
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
FROM node:${NODE_VERSION}-bookworm-slim AS node-tools
ARG SPLAT_TRANSFORM_VERSION=3.11.2
# グローバルインストールすると /usr/local/lib/node_modules に入る。
# 実行用イメージには node 本体とこのディレクトリだけをコピーする。
RUN npm install -g --omit=dev "@playcanvas/splat-transform@${SPLAT_TRANSFORM_VERSION}" \
    && npm cache clean --force

# ----------------------------------------------------------------------------
# 実行用ベース（外部CLI一式）
# ----------------------------------------------------------------------------
FROM ubuntu:${UBUNTU_VERSION} AS tools
ENV DEBIAN_FRONTEND=noninteractive
# 実行時に必要な共有ライブラリだけを入れる（-dev パッケージは入れない）。
# 一覧は COLMAP 公式 Dockerfile の runtime ステージを基に、Qt/MKL を外し、
# CGAL（gmp/mpfr）と OpenSplat 用の OpenCV を足したもの。
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tini \
        libboost-program-options1.83.0 libgomp1 libopengl0 libgl1 libglew2.2 \
        libmetis5 libceres4t64 libopenimageio2.4t64 libgoogle-glog0v6t64 \
        libsqlite3-0 libgmp10 libmpfr6 \
        libopencv-core406t64 libopencv-imgproc406t64 libopencv-imgcodecs406t64 \
        libopencv-calib3d406t64 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=colmap-build /opt/colmap/bin/colmap /usr/local/bin/colmap
COPY --from=opensplat-build /opt/opensplat/bin/opensplat /usr/local/bin/opensplat
COPY --from=opensplat-build /opt/pytorch /opt/pytorch
COPY --from=gltfpack-build /opt/gltfpack/bin/gltfpack /usr/local/bin/gltfpack
COPY --from=node-tools /usr/local/bin/node /usr/local/bin/node
COPY --from=node-tools /usr/local/lib/node_modules/@playcanvas/splat-transform /usr/local/lib/node_modules/@playcanvas/splat-transform

# - libtorch の場所を動的リンカに教える（LD_LIBRARY_PATH に頼らない）。
# - splat-transform の実行ファイル名を package.json の bin から解決して PATH に置く。
# - 最後に「共有ライブラリが1つでも見つからなければビルド失敗」にする。
#   これを入れておくと、パッケージの入れ忘れが実行時（＝ユーザーの復元ジョブ中）ではなく
#   イメージのビルド時に見つかる。
RUN set -eu; \
    { echo /opt/pytorch/torch/lib; [ -d /opt/pytorch/torch.libs ] && echo /opt/pytorch/torch.libs; } > /etc/ld.so.conf.d/pytorch.conf; \
    ldconfig; \
    bin_rel="$(node -e 'const b=require("/usr/local/lib/node_modules/@playcanvas/splat-transform/package.json").bin; console.log(typeof b==="string"?b:b["splat-transform"])')"; \
    ln -s "/usr/local/lib/node_modules/@playcanvas/splat-transform/${bin_rel}" /usr/local/bin/splat-transform; \
    chmod +x "/usr/local/lib/node_modules/@playcanvas/splat-transform/${bin_rel}"; \
    for b in colmap opensplat gltfpack node; do \
        if ldd "/usr/local/bin/$b" | grep -q "not found"; then \
            echo "共有ライブラリが不足しています: $b"; ldd "/usr/local/bin/$b" | grep "not found"; exit 1; \
        fi; \
    done; \
    colmap help >/dev/null; opensplat --version; gltfpack -h >/dev/null 2>&1 || true; splat-transform --version

# ----------------------------------------------------------------------------
# Web（Vite + React + TypeScript）
# ----------------------------------------------------------------------------
FROM node:${NODE_VERSION}-bookworm-slim AS web-build
WORKDIR /src/web
# 依存関係の層とソースの層を分ける: ソースだけ変えた再ビルドで npm ci をやり直さずに済む。
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ----------------------------------------------------------------------------
# Go（API + ワーカー）
# ----------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS server-build
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
# CGO_ENABLED=0: C ライブラリに依存しない単一バイナリにする（実行イメージの glibc の版に左右されない）。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scanner-server ./cmd/server

# ----------------------------------------------------------------------------
# 最終イメージ
# ----------------------------------------------------------------------------
FROM tools AS runtime
# root で動かさない。UID を固定しておくと、Kubernetes 側で fsGroup を同じ値にして
# PVC（/data）への書き込み権限を与えられる。
# （ubuntu:24.04 には UID 1000 の ubuntu ユーザーが最初から居るので、衝突しない番号にする）
RUN useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin scanner \
    && mkdir -p /app/web /data \
    && chown -R scanner:scanner /data
COPY --from=server-build /out/scanner-server /app/scanner-server
COPY --from=web-build /src/web/dist /app/web

ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/data \
    WEB_DIR=/app/web \
    # splat-transform が設定やキャッシュを書こうとしても読み取り専用のルートFSで困らないようにする。
    HOME=/tmp
USER 10001:10001
WORKDIR /app
EXPOSE 8080
VOLUME ["/data"]
# tini を PID 1 にする理由: Go サーバーが起動した COLMAP 等の子プロセスが孤児になっても
# 回収（reap）され、SIGTERM も正しく転送される。
ENTRYPOINT ["/usr/bin/tini", "--", "/app/scanner-server"]
