-- 0002_gallery.sql: ギャラリー（完成した3Dデータを残しておく機能）のための列。
--
-- 0001 では「すべてのスキャンを保存期限（既定24時間）で消す」方針だった。
-- ギャラリーを足したので、完成したもの（succeeded / partial）は saved = true にして期限で消さない。
-- 失敗・中止したものはこれまでどおり期限で消える（写真を理由なく残し続けない）。

-- saved: ギャラリーに保存されている。true の行は期限切れの掃除の対象から外れる。
ALTER TABLE scans ADD COLUMN saved BOOLEAN NOT NULL DEFAULT false;
-- title: 利用者が付けた名前。空なら画面は撮影日時を名前の代わりに出す。
ALTER TABLE scans ADD COLUMN title TEXT NOT NULL DEFAULT '' CHECK (char_length(title) <= 80);

-- この移行より前に完成していて、まだ消えていないものもギャラリーに入れる。
UPDATE scans SET saved = true WHERE status IN ('succeeded', 'partial');

-- 一覧（新しい順、期限切れを除く）用の部分インデックス。
CREATE INDEX scans_list_idx ON scans (created_at DESC, id DESC) WHERE status <> 'expired';
