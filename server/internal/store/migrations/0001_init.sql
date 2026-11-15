-- 0001_init.sql: スキャン（復元ジョブ）を管理する4つのテーブル。
--
-- ここに入れるのは「ジョブの状態と成果物の目録」だけで、写真や3Dモデルの本体は PVC 上の
-- ファイルに置く。DB を小さく保て、バックアップや期限切れの削除も単純になる。
-- （COLMAP が内部で使う database.db は SQLite で、この PostgreSQL とは別物。）

CREATE TABLE scans (
    -- アプリ側で生成した UUID。作業ディレクトリ名（/data/scans/<id>）にもなる。
    id            UUID PRIMARY KEY,
    -- 状態の一覧は server/internal/scan/scan.go と一致させる。CHECK 制約にしておくと、
    -- プログラムの打ち間違いで未知の状態が入るのを DB が止めてくれる。
    status        TEXT        NOT NULL CHECK (status IN
                      ('queued', 'running', 'partial', 'succeeded', 'failed', 'cancelled', 'expired')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- この時刻を過ぎたら画像と成果物を削除する（MVP は長期保存しない）。
    expires_at    TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    frame_count   INT         NOT NULL CHECK (frame_count >= 0),
    -- ワーカーが着手した回数。再起動で中断されたジョブを無限にやり直さないための歯止め。
    attempts      INT         NOT NULL DEFAULT 0,
    error_code    TEXT        NOT NULL DEFAULT '',
    error_message TEXT        NOT NULL DEFAULT '',
    -- 撮影側が送った付加情報（端末・ブラウザ等）。JSONB なので、VPS/ARCore を足すときに
    -- 列を増やさず項目を追加できる。
    capture_meta  JSONB       NOT NULL DEFAULT '{}'::jsonb
);

-- 待ち行列の先頭を探す問い合わせ（status='queued' を古い順）専用の部分インデックス。
-- queued の行だけを含むので小さく、終了済みジョブが増えても速さが変わらない。
CREATE INDEX scans_queue_idx ON scans (created_at, id) WHERE status = 'queued';
-- 期限切れの掃除（expires_at が過去の行）用。
CREATE INDEX scans_expires_idx ON scans (expires_at);

-- 写真1枚ごとの記録。MVP では枚数と寸法の確認にしか使わないが、
-- captured_at（撮影時刻）と camera（焦点距離、将来は端末の姿勢など）を持たせてあるので、
-- 再訪問スキャンや VPS を足すときの土台になる。
CREATE TABLE scan_frames (
    scan_id     UUID   NOT NULL REFERENCES scans (id) ON DELETE CASCADE,
    frame_index INT    NOT NULL,
    -- サーバーが付けた名前（frame_0001.jpg）。利用者のファイル名は保存しない。
    file_name   TEXT   NOT NULL,
    width       INT    NOT NULL,
    height      INT    NOT NULL,
    bytes       BIGINT NOT NULL,
    sha256      TEXT   NOT NULL,
    captured_at TIMESTAMPTZ,
    camera      JSONB  NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (scan_id, frame_index)
);

-- 復元パイプラインの各段階（sfm / mvs / mesh / splat / sog）の状態。
-- 分岐ごとに成功・失敗を独立に記録できるので「Mesh は成功、3DGS は失敗」を表現できる。
CREATE TABLE scan_steps (
    scan_id       UUID NOT NULL REFERENCES scans (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    -- 画面に並べる順番。
    position      INT  NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped')),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    message       TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (scan_id, name)
);

-- 配信できる成果物の目録。ファイルの存在とサイズを検証できたものだけを登録する。
CREATE TABLE scan_assets (
    scan_id       UUID   NOT NULL REFERENCES scans (id) ON DELETE CASCADE,
    kind          TEXT   NOT NULL,
    -- 作業ディレクトリからの相対パス。絶対パスは保存しない。
    relative_path TEXT   NOT NULL,
    size_bytes    BIGINT NOT NULL CHECK (size_bytes > 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scan_id, kind)
);
