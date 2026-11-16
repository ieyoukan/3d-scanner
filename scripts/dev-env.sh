#!/usr/bin/env bash
# ローカル開発用の .env を作る。秘密の値はその場で乱数から生成し、リポジトリには含めない。
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -e .env ]; then
  echo ".env は既にあります。作り直す場合は先に削除してください。" >&2
  exit 1
fi

# openssl rand -hex 24 は 48 文字の16進文字列（192ビットの乱数）を出す。
token="$(openssl rand -hex 24)"
db_password="$(openssl rand -hex 24)"

# umask 077: これから作るファイルを、自分だけが読める権限（600）にする。
umask 077
cat > .env <<ENV
ACCESS_TOKEN=${token}
POSTGRES_PASSWORD=${db_password}
ENV

echo ".env を作りました。"
echo "画面で入力するアクセストークンは .env の ACCESS_TOKEN です（cat .env で確認できます）。"
