-- 0004_splat_jobs.sql: 3DGS を「あとで」作るための待ち行列。
--
-- メッシュまで出来た時点でスキャンを完成にし、3DGS の学習はこの待ち行列に入れて、手が空いたときに行う。
-- CPU での学習は数十分かかるので、新しいスキャンの位置推定とメッシュを待たせないため。
-- GPU を使えるようになったら、同じ待ち行列から作り直すこともできる。
--
-- 行があるのは待っている間と実行中だけ。終わったら消す（結果は scan_steps と scan_assets に残る）。
CREATE TABLE splat_jobs (
    scan_id    UUID PRIMARY KEY REFERENCES scans (id) ON DELETE CASCADE,
    status     TEXT        NOT NULL CHECK (status IN ('queued', 'running')),
    queued_at  TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    -- 着手した回数。再起動で中断された学習を、無限にやり直さないための歯止め。
    attempts   INT         NOT NULL DEFAULT 0
);
CREATE INDEX splat_jobs_queue_idx ON splat_jobs (queued_at, scan_id) WHERE status = 'queued';
