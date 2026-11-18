-- 0003_resume.sql: 「続きを撮る」（保存済みの3Dデータに写真を足して作り直す）ための列。
--
-- 続きのスキャンは新しい行として作り、元の版を parent_id で指す。元の版の行とファイルは書き換えないので、
-- 追加撮影が失敗しても前の3Dデータはそのまま残る。
-- ON DELETE SET NULL: 元の版を削除しても続きの版は消さない（写真と地図は受付時に引き継ぎ済みで、
-- 元の版のファイルが無くても処理と閲覧ができる）。
ALTER TABLE scans ADD COLUMN parent_id UUID REFERENCES scans (id) ON DELETE SET NULL;
-- 元の版から引き継いだ写真の枚数（frame_count に含まれる）。画面で「前回◯枚＋今回◯枚」と示すのに使う。
ALTER TABLE scans ADD COLUMN base_frame_count INT NOT NULL DEFAULT 0 CHECK (base_frame_count >= 0);
-- 「この版の続き」を探す問い合わせ（一覧で古い版を隠す、画面に続きの版を出す）用。
CREATE INDEX scans_parent_idx ON scans (parent_id) WHERE parent_id IS NOT NULL;
