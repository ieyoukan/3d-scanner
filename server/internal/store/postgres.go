package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	// pgx を database/sql のドライバとして登録する（"pgx" という名前で使えるようになる）。
	// database/sql 経由にしているのは、Go 標準の書き方のまま学べるようにするため。
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// Postgres は PostgreSQL を使う Store の実装。
type Postgres struct {
	db *sql.DB
}

// OpenPostgres は接続プールを作り、疎通を確かめて返す。
//
// 入力: 接続文字列（postgres://user:pass@host:5432/db?sslmode=disable）。
// 副作用: DB への接続を開く。スキーマは変更しない（Migrate を別に呼ぶ）。
// 失敗条件: 文字列が不正、または ctx の期限内に接続できない。
func OpenPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("DB の接続設定が不正です: %w", err)
	}
	// このアプリは同時に「HTTP の数リクエスト＋ワーカー1つ」しか DB を使わない。
	// 小さな PostgreSQL の接続数上限を食い潰さないよう控えめにする。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("DB に接続できません: %w", err)
	}
	return &Postgres{db: db}, nil
}

// DB はマイグレーション用に内部の *sql.DB を返す。
func (p *Postgres) DB() *sql.DB { return p.db }

func (p *Postgres) Close() error { return p.db.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

// inTx は fn をトランザクションの中で実行する。fn がエラーを返せば全て取り消す。
func (p *Postgres) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// Rollback 自体の失敗は元のエラーほど重要ではないので捨てる。
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (p *Postgres) Create(ctx context.Context, s scan.Scan, frames []scan.Frame) error {
	return p.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO scans (id, status, created_at, updated_at, expires_at, frame_count, capture_meta)
			VALUES ($1, $2, $3, $3, $4, $5, $6::jsonb)`,
			s.ID, string(scan.StatusQueued), s.CreatedAt, s.ExpiresAt, s.FrameCount, jsonOrEmpty(s.CaptureMeta))
		if err != nil {
			return fmt.Errorf("scans への登録に失敗: %w", err)
		}
		for _, f := range frames {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO scan_frames (scan_id, frame_index, file_name, width, height, bytes, sha256, captured_at, camera)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
				s.ID, f.Index, f.FileName, f.Width, f.Height, f.Bytes, f.SHA256, nullTime(f.CapturedAt), jsonOrEmpty(f.Camera))
			if err != nil {
				return fmt.Errorf("scan_frames への登録に失敗: %w", err)
			}
		}
		for i, name := range scan.StepOrder {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO scan_steps (scan_id, name, position, status) VALUES ($1, $2, $3, $4)`,
				s.ID, string(name), i, string(scan.StepPending))
			if err != nil {
				return fmt.Errorf("scan_steps への登録に失敗: %w", err)
			}
		}
		return nil
	})
}

// scanColumns は scans を読むときの列の並び。scanRow と必ず同じ順番にする。
const scanColumns = `id::text, status, created_at, updated_at, expires_at, started_at, finished_at,
	frame_count, attempts, error_code, error_message, capture_meta, saved, title`

// rowScanner は *sql.Row と *sql.Rows の共通部分（Scan だけ）を表す。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(r rowScanner) (scan.Scan, error) {
	var s scan.Scan
	var status string
	var started, finished sql.NullTime
	var meta []byte
	err := r.Scan(&s.ID, &status, &s.CreatedAt, &s.UpdatedAt, &s.ExpiresAt, &started, &finished,
		&s.FrameCount, &s.Attempts, &s.ErrorCode, &s.ErrorMessage, &meta, &s.Saved, &s.Title)
	if err != nil {
		return scan.Scan{}, err
	}
	s.Status = scan.Status(status)
	s.StartedAt = timePtr(started)
	s.FinishedAt = timePtr(finished)
	s.CaptureMeta = json.RawMessage(meta)
	return s, nil
}

func (p *Postgres) Get(ctx context.Context, id string) (scan.Detail, error) {
	var d scan.Detail
	s, err := scanRow(p.db.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM scans WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Scan = s

	stepRows, err := p.db.QueryContext(ctx, `
		SELECT name, status, started_at, finished_at, message, error_message
		FROM scan_steps WHERE scan_id = $1 ORDER BY position`, id)
	if err != nil {
		return d, err
	}
	defer stepRows.Close()
	for stepRows.Next() {
		var st scan.Step
		var name, status string
		var started, finished sql.NullTime
		if err := stepRows.Scan(&name, &status, &started, &finished, &st.Message, &st.ErrorMessage); err != nil {
			return d, err
		}
		st.Name, st.Status = scan.StepName(name), scan.StepStatus(status)
		st.StartedAt, st.FinishedAt = timePtr(started), timePtr(finished)
		d.Steps = append(d.Steps, st)
	}
	if err := stepRows.Err(); err != nil {
		return d, err
	}

	assetRows, err := p.db.QueryContext(ctx, `
		SELECT kind, relative_path, size_bytes FROM scan_assets WHERE scan_id = $1 ORDER BY kind`, id)
	if err != nil {
		return d, err
	}
	defer assetRows.Close()
	for assetRows.Next() {
		var a scan.Asset
		var kind string
		if err := assetRows.Scan(&kind, &a.RelativePath, &a.SizeBytes); err != nil {
			return d, err
		}
		a.Kind = scan.AssetKind(kind)
		d.Assets = append(d.Assets, a)
	}
	return d, assetRows.Err()
}

func (p *Postgres) CountActive(ctx context.Context) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM scans WHERE status IN ('queued', 'running')`).Scan(&n)
	return n, err
}

func (p *Postgres) QueuePosition(ctx context.Context, id string) (int, error) {
	var n int
	// 並び順は ClaimNext と同じ（created_at, id）。自分より前にいる queued を数える。
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM scans o, scans me
		WHERE me.id = $1 AND o.status = 'queued' AND (o.created_at, o.id) < (me.created_at, me.id)`, id).Scan(&n)
	return n, err
}

func (p *Postgres) ClaimNext(ctx context.Context, now time.Time) (*scan.Scan, error) {
	// この1文がジョブの排他の要。
	//   内側の SELECT ... FOR UPDATE SKIP LOCKED:
	//     最も古い queued の行をロックして選ぶ。他のトランザクションが既にロックしている行は
	//     待たずに飛ばす（SKIP LOCKED）。だから2つのワーカーが同時に実行しても別々の行を取る。
	//   外側の UPDATE:
	//     選んだ行を running にする。選ぶのと更新が同じ文なので、間に割り込まれない。
	// MVP のワーカーは1つだけだが、将来ワーカーを増やしてもこの問い合わせはそのまま使える。
	s, err := scanRow(p.db.QueryRowContext(ctx, `
		UPDATE scans
		SET status = 'running', started_at = $1, updated_at = $1, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM scans WHERE status = 'queued'
			ORDER BY created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+scanColumns, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (p *Postgres) UpdateStep(ctx context.Context, scanID string, st scan.Step) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE scan_steps
		SET status = $3, started_at = $4, finished_at = $5, message = $6, error_message = $7
		WHERE scan_id = $1 AND name = $2`,
		scanID, string(st.Name), string(st.Status), nullTime(st.StartedAt), nullTime(st.FinishedAt),
		st.Message, st.ErrorMessage)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

func (p *Postgres) PutAsset(ctx context.Context, scanID string, a scan.Asset) error {
	// ON CONFLICT ... DO UPDATE（いわゆる upsert）: やり直しで同じ種類を作り直しても重複しない。
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO scan_assets (scan_id, kind, relative_path, size_bytes) VALUES ($1, $2, $3, $4)
		ON CONFLICT (scan_id, kind) DO UPDATE
		SET relative_path = EXCLUDED.relative_path, size_bytes = EXCLUDED.size_bytes, created_at = now()`,
		scanID, string(a.Kind), a.RelativePath, a.SizeBytes)
	return err
}

func (p *Postgres) List(ctx context.Context, q ListQuery) ([]scan.Summary, error) {
	// 成果物の種類は string_agg で1つの文字列（"mesh_glb,scene_json"）にまとめて読む。
	// 一覧の各行で scan_assets を別に問い合わせると、件数ぶん往復が増える（N+1 問題）。
	query := `SELECT ` + scanColumns + `,
		COALESCE((SELECT string_agg(kind, ',' ORDER BY kind) FROM scan_assets a WHERE a.scan_id = s.id), '')
		FROM scans s WHERE status <> 'expired'`
	args := []any{}
	if q.After != nil {
		// (created_at, id) の組で比べる（行値の比較）。同じ時刻の行があっても順番が一意に決まる。
		query += ` AND (created_at, id) < ($1, $2::uuid)`
		args = append(args, q.After.CreatedAt, q.After.ID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, q.Limit)

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scan.Summary
	for rows.Next() {
		var sum scan.Summary
		var status, kinds string
		var started, finished sql.NullTime
		var meta []byte
		sc := &sum.Scan
		if err := rows.Scan(&sc.ID, &status, &sc.CreatedAt, &sc.UpdatedAt, &sc.ExpiresAt, &started, &finished,
			&sc.FrameCount, &sc.Attempts, &sc.ErrorCode, &sc.ErrorMessage, &meta, &sc.Saved, &sc.Title, &kinds); err != nil {
			return nil, err
		}
		sc.Status = scan.Status(status)
		sc.StartedAt, sc.FinishedAt = timePtr(started), timePtr(finished)
		sc.CaptureMeta = json.RawMessage(meta)
		for _, k := range strings.Split(kinds, ",") {
			if k != "" {
				sum.AssetKinds = append(sum.AssetKinds, scan.AssetKind(k))
			}
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

func (p *Postgres) SetTitle(ctx context.Context, id, title string, now time.Time) error {
	res, err := p.db.ExecContext(ctx, `UPDATE scans SET title = $2, updated_at = $3 WHERE id = $1 AND status <> 'expired'`, id, title, now)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

func (p *Postgres) Finish(ctx context.Context, id string, out scan.Outcome, now time.Time) (bool, error) {
	// WHERE status = 'running' が守り。キャンセル済み（cancelled）の行は更新されない。
	res, err := p.db.ExecContext(ctx, `
		UPDATE scans
		SET status = $2, error_code = $3, error_message = $4, finished_at = $5, updated_at = $5, saved = $6
		WHERE id = $1 AND status = 'running'`,
		id, string(out.Status), out.ErrorCode, out.ErrorMessage, now, out.Saved)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (p *Postgres) Cancel(ctx context.Context, id string, now time.Time) (scan.Status, bool, error) {
	var prev scan.Status
	var changed bool
	err := p.inTx(ctx, func(tx *sql.Tx) error {
		var status string
		// FOR UPDATE で行をロックしてから状態を見る。読んだ直後にワーカーが Finish しても、
		// こちらのトランザクションが終わるまで待たされるので、判断が食い違わない。
		err := tx.QueryRowContext(ctx, `SELECT status FROM scans WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		prev = scan.Status(status)
		if !prev.Active() {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE scans SET status = 'cancelled', finished_at = $2, updated_at = $2,
			       error_code = 'cancelled', error_message = '利用者が中止しました。'
			WHERE id = $1`, id, now); err != nil {
			return err
		}
		// 実行中・未着手のステップは「実行しなかった」に揃える（画面に回り続ける表示を残さない）。
		if _, err := tx.ExecContext(ctx, `
			UPDATE scan_steps SET status = 'skipped', finished_at = $2, message = '中止されました。'
			WHERE scan_id = $1 AND status IN ('pending', 'running')`, id, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM scan_assets WHERE scan_id = $1`, id); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return prev, changed, err
}

func (p *Postgres) Delete(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM scans WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

func (p *Postgres) Exists(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM scans WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

func (p *Postgres) RecoverInterrupted(ctx context.Context, maxAttempts int, now time.Time) (requeued, failed []string, err error) {
	err = p.inTx(ctx, func(tx *sql.Tx) error {
		// 1) やり直しの上限に達したものは failed にする。
		failed, err = collectIDs(tx.QueryContext(ctx, `
			UPDATE scans
			SET status = 'failed', error_code = $2, error_message = $3, finished_at = $4, updated_at = $4
			WHERE status = 'running' AND attempts >= $1
			RETURNING id::text`, maxAttempts, interruptedCode, interruptedMessage, now))
		if err != nil {
			return err
		}
		for _, id := range failed {
			if _, err := tx.ExecContext(ctx, `
				UPDATE scan_steps SET status = 'failed', finished_at = $2, error_message = $3
				WHERE scan_id = $1 AND status = 'running'`, id, now, interruptedStep); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE scan_steps SET status = 'skipped', finished_at = $2, message = $3
				WHERE scan_id = $1 AND status = 'pending'`, id, now, notRunStep); err != nil {
				return err
			}
		}

		// 2) 残りの running は queued に戻す。1) の後なので attempts < maxAttempts のものだけが残っている。
		requeued, err = collectIDs(tx.QueryContext(ctx, `
			UPDATE scans SET status = 'queued', started_at = NULL, updated_at = $1
			WHERE status = 'running'
			RETURNING id::text`, now))
		if err != nil {
			return err
		}
		for _, id := range requeued {
			// 最初からやり直すので、途中まで進んだ記録は白紙に戻す。
			if _, err := tx.ExecContext(ctx, `
				UPDATE scan_steps
				SET status = 'pending', started_at = NULL, finished_at = NULL, message = '', error_message = ''
				WHERE scan_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM scan_assets WHERE scan_id = $1`, id); err != nil {
				return err
			}
		}
		return nil
	})
	return requeued, failed, err
}

func (p *Postgres) ListExpired(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return collectIDs(p.db.QueryContext(ctx, `
		SELECT id::text FROM scans
		WHERE expires_at < $1 AND status <> 'expired' AND NOT saved
		ORDER BY expires_at LIMIT $2`, now, limit))
}

func (p *Postgres) MarkExpired(ctx context.Context, id string, now time.Time) error {
	return p.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE scans SET status = 'expired', updated_at = $2,
			       finished_at = COALESCE(finished_at, $2)
			WHERE id = $1`, id, now)
		if err != nil {
			return err
		}
		if err := requireOneRow(res); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM scan_assets WHERE scan_id = $1`, id)
		return err
	})
}

func (p *Postgres) PurgeExpired(ctx context.Context, expiredBefore time.Time) (int, error) {
	res, err := p.db.ExecContext(ctx, `DELETE FROM scans WHERE status = 'expired' AND expires_at < $1`, expiredBefore)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- 小さな補助関数 ----

// collectIDs は「id を1列だけ返す問い合わせ」の結果を []string にまとめる。
// QueryContext の戻り値（rows, err）をそのまま渡せるよう、エラーも引数で受ける。
func collectIDs(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// requireOneRow は更新・削除が1行に当たったことを確かめる。0行なら対象が無い。
func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// nullTime は *time.Time を DB の NULL 許容の値に変換する。
func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// jsonOrEmpty は空の JSON を "{}" に揃える（JSONB 列は NOT NULL なので NULL を入れない）。
func jsonOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
