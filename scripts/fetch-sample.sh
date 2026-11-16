#!/usr/bin/env bash
# 動作確認用のサンプル写真（11枚）をダウンロードする。
#
# 出典: openMVG/ImageDataset_SceauxCastle（フランス・ソー城の写真）
#       Photographer: Copyright 2012 Pierre MOULON
# このリポジトリにライセンスの明記は無いため、写真そのものは本リポジトリに含めず、
# 動作確認のたびに元の場所から取得する。再配布や公開物への利用は避けること。
set -euo pipefail

dest="${1:-samples/sceaux/images}"
base="https://raw.githubusercontent.com/openMVG/ImageDataset_SceauxCastle/master/images"

mkdir -p "$dest"
for n in 7100 7101 7102 7103 7104 7105 7106 7107 7108 7109 7110; do
  file="100_${n}.JPG"
  if [ ! -s "$dest/$file" ]; then
    echo "取得: $file"
    curl -fsSL --retry 3 -o "$dest/$file" "$base/$file"
  fi
done
echo "サンプル写真を $dest に用意しました（$(ls "$dest" | wc -l | tr -d ' ') 枚）。"
