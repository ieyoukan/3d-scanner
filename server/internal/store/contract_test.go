package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// このファイルは「Store の決まり」を1か所に書き、Memory と Postgres の両方に流す（契約テスト）。
// テスト用の Memory が本番の Postgres と違う動きをすると、ハンドラやワーカーのテストが
// 通っても本番で壊れる。同じテストを両方に流すことでそれを防ぐ。
//
// Postgres 側は環境変数 TEST_DATABASE_URL があるときだけ実行する:
//   TEST_DATABASE_URL=postgres://scanner:scanner@localhost:5432/scanner_test?sslmode=disable go test ./internal/store/
// 注意: テストは scans テーブルを空にする。必ず専用のデータベースを指定すること。

func TestMemoryContract(t *testing.T) {
	runContract(t, func(t *testing.T) Store { return NewMemory() })
}

func TestPostgresContract(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL が未設定のため Postgres の契約テストを省略")
	}
	ctx := context.Background()
	pg, err := OpenPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	if _, err := Migrate(ctx, pg.DB()); err != nil {
		t.Fatal(err)
	}
	// 2回目の Migrate は何も適用しないこと（冪等性）もここで確かめる。
	applied, err := Migrate(ctx, pg.DB())
	if err != nil || len(applied) != 0 {
		t.Fatalf("2回目の Migrate: applied=%v err=%v（何も適用しないはず）", applied, err)
	}
	runContract(t, func(t *testing.T) Store {
		if _, err := pg.DB().ExecContext(ctx, `TRUNCATE scans CASCADE`); err != nil {
			t.Fatal(err)
		}
		return pg
	})
}

// base はテストで使う基準時刻。DB はマイクロ秒精度なので、ナノ秒の端数が無い値にする。
var base = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// newScan は ID だけ違うテスト用スキャンを作る。offset は作成時刻のずらし（並び順の制御）。
func newScan(t *testing.T, offset time.Duration) scan.Scan {
	t.Helper()
	id, err := scan.NewID()
	if err != nil {
		t.Fatal(err)
	}
	created := base.Add(offset)
	return scan.Scan{ID: id, CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour), FrameCount: 2}
}

func mustCreate(t *testing.T, s Store, sc scan.Scan) {
	t.Helper()
	captured := sc.CreatedAt.Add(-time.Minute)
	frames := []scan.Frame{
		{Index: 0, FileName: "frame_0001.jpg", Width: 1920, Height: 1080, Bytes: 1000, SHA256: "aa", CapturedAt: &captured},
		{Index: 1, FileName: "frame_0002.jpg", Width: 1920, Height: 1080, Bytes: 1100, SHA256: "bb"},
	}
	if err := s.Create(context.Background(), sc, frames); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func mustGet(t *testing.T, s Store, id string) scan.Detail {
	t.Helper()
	d, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return d
}

func stepStatus(d scan.Detail, name scan.StepName) scan.StepStatus {
	for _, st := range d.Steps {
		if st.Name == name {
			return st.Status
		}
	}
	return ""
}

func runContract(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("作成直後は queued で全ステップが pending", func(t *testing.T) {
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)

		d := mustGet(t, s, sc.ID)
		if d.Scan.Status != scan.StatusQueued {
			t.Fatalf("status = %q", d.Scan.Status)
		}
		if d.Scan.FrameCount != 2 || !d.Scan.ExpiresAt.Equal(sc.ExpiresAt) {
			t.Fatalf("保存した値が違う: %+v", d.Scan)
		}
		if len(d.Steps) != len(scan.StepOrder) {
			t.Fatalf("ステップ数 = %d", len(d.Steps))
		}
		for i, st := range d.Steps {
			if st.Name != scan.StepOrder[i] || st.Status != scan.StepPending {
				t.Fatalf("ステップ %d = %+v", i, st)
			}
		}
		if len(d.Assets) != 0 {
			t.Fatalf("成果物が最初からある: %v", d.Assets)
		}
	})

	t.Run("無い ID は ErrNotFound", func(t *testing.T) {
		s := open(t)
		id, _ := scan.NewID()
		if _, err := s.Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		if _, _, err := s.Cancel(ctx, id, base); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Cancel err = %v", err)
		}
		if err := s.Delete(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete err = %v", err)
		}
	})

	t.Run("ClaimNext は古い順に1件ずつ running にする", func(t *testing.T) {
		s := open(t)
		older, newer := newScan(t, 0), newScan(t, time.Second)
		mustCreate(t, s, newer) // 登録順ではなく作成時刻で並ぶことを確かめるため、新しい方を先に入れる
		mustCreate(t, s, older)

		if pos, _ := s.QueuePosition(ctx, newer.ID); pos != 1 {
			t.Fatalf("newer の待ち順 = %d, want 1", pos)
		}

		got, err := s.ClaimNext(ctx, base.Add(time.Minute))
		if err != nil || got == nil {
			t.Fatalf("ClaimNext: %v %v", got, err)
		}
		if got.ID != older.ID || got.Status != scan.StatusRunning || got.Attempts != 1 || got.StartedAt == nil {
			t.Fatalf("最初に取れたジョブが違う: %+v", got)
		}
		if pos, _ := s.QueuePosition(ctx, newer.ID); pos != 0 {
			t.Fatalf("先頭が running になった後の待ち順 = %d, want 0", pos)
		}
		if n, _ := s.CountActive(ctx); n != 2 {
			t.Fatalf("CountActive = %d, want 2（running 1 + queued 1）", n)
		}

		got2, _ := s.ClaimNext(ctx, base.Add(2*time.Minute))
		if got2 == nil || got2.ID != newer.ID {
			t.Fatalf("2件目が違う: %+v", got2)
		}
		got3, err := s.ClaimNext(ctx, base.Add(3*time.Minute))
		if err != nil || got3 != nil {
			t.Fatalf("待ちが無いのに取れた: %+v %v", got3, err)
		}
	})

	t.Run("ステップと成果物を更新できる", func(t *testing.T) {
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)
		s.ClaimNext(ctx, base)

		started := base.Add(time.Second)
		finished := base.Add(time.Minute)
		st := scan.Step{Name: scan.StepSfM, Status: scan.StepSucceeded, StartedAt: &started, FinishedAt: &finished, Message: "2枚中2枚"}
		if err := s.UpdateStep(ctx, sc.ID, st); err != nil {
			t.Fatal(err)
		}
		if err := s.PutAsset(ctx, sc.ID, scan.Asset{Kind: scan.AssetSparsePLY, RelativePath: "output/sparse.ply", SizeBytes: 10}); err != nil {
			t.Fatal(err)
		}
		// 同じ種類をもう一度登録すると置き換わる（重複しない）。
		if err := s.PutAsset(ctx, sc.ID, scan.Asset{Kind: scan.AssetSparsePLY, RelativePath: "output/sparse.ply", SizeBytes: 20}); err != nil {
			t.Fatal(err)
		}

		d := mustGet(t, s, sc.ID)
		got := d.Steps[0]
		if got.Status != scan.StepSucceeded || got.Message != "2枚中2枚" || got.StartedAt == nil || !got.FinishedAt.Equal(finished) {
			t.Fatalf("ステップが保存されていない: %+v", got)
		}
		if len(d.Assets) != 1 || d.Assets[0].SizeBytes != 20 {
			t.Fatalf("成果物 = %+v", d.Assets)
		}
	})

	t.Run("Finish は running のときだけ効く", func(t *testing.T) {
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)

		// まだ queued なので Finish は何もしない。
		ok, err := s.Finish(ctx, sc.ID, scan.Outcome{Status: scan.StatusSucceeded}, base)
		if err != nil || ok {
			t.Fatalf("queued に Finish が効いた: ok=%v err=%v", ok, err)
		}

		s.ClaimNext(ctx, base)
		ok, err = s.Finish(ctx, sc.ID, scan.Outcome{Status: scan.StatusPartial, ErrorCode: "splat_failed", ErrorMessage: "一部失敗"}, base.Add(time.Hour))
		if err != nil || !ok {
			t.Fatalf("Finish: ok=%v err=%v", ok, err)
		}
		d := mustGet(t, s, sc.ID)
		if d.Scan.Status != scan.StatusPartial || d.Scan.ErrorCode != "splat_failed" || d.Scan.FinishedAt == nil {
			t.Fatalf("終了状態が保存されていない: %+v", d.Scan)
		}
	})

	t.Run("キャンセル後の Finish は結果を上書きしない", func(t *testing.T) {
		// 利用者の中止とワーカーの完了がほぼ同時に起きた場合の決まり。
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)
		s.ClaimNext(ctx, base)
		running := base
		s.UpdateStep(ctx, sc.ID, scan.Step{Name: scan.StepSfM, Status: scan.StepRunning, StartedAt: &running})
		s.PutAsset(ctx, sc.ID, scan.Asset{Kind: scan.AssetSparsePLY, RelativePath: "output/sparse.ply", SizeBytes: 10})

		prev, changed, err := s.Cancel(ctx, sc.ID, base.Add(time.Minute))
		if err != nil || !changed || prev != scan.StatusRunning {
			t.Fatalf("Cancel: prev=%q changed=%v err=%v", prev, changed, err)
		}
		ok, _ := s.Finish(ctx, sc.ID, scan.Outcome{Status: scan.StatusSucceeded}, base.Add(2*time.Minute))
		if ok {
			t.Fatal("cancelled に Finish が効いてしまった")
		}

		d := mustGet(t, s, sc.ID)
		if d.Scan.Status != scan.StatusCancelled {
			t.Fatalf("status = %q", d.Scan.Status)
		}
		if got := stepStatus(d, scan.StepSfM); got != scan.StepSkipped {
			t.Fatalf("実行中だったステップ = %q, want skipped", got)
		}
		if len(d.Assets) != 0 {
			t.Fatalf("中止後に成果物が残っている: %v", d.Assets)
		}

		// 既に終了したものをもう一度中止しても何も変わらない。
		prev, changed, err = s.Cancel(ctx, sc.ID, base.Add(3*time.Minute))
		if err != nil || changed || prev != scan.StatusCancelled {
			t.Fatalf("2回目の Cancel: prev=%q changed=%v err=%v", prev, changed, err)
		}
	})

	t.Run("起動時の回復: 上限未満はやり直し、上限到達は失敗", func(t *testing.T) {
		s := open(t)
		retry, giveUp, untouched := newScan(t, 0), newScan(t, time.Second), newScan(t, 2*time.Second)
		mustCreate(t, s, retry)
		mustCreate(t, s, giveUp)
		mustCreate(t, s, untouched)

		// retry: 1回着手して中断された状態を作る。
		s.ClaimNext(ctx, base)
		running := base
		s.UpdateStep(ctx, retry.ID, scan.Step{Name: scan.StepSfM, Status: scan.StepSucceeded, StartedAt: &running, FinishedAt: &running})
		s.UpdateStep(ctx, retry.ID, scan.Step{Name: scan.StepMesh, Status: scan.StepRunning, StartedAt: &running})
		s.PutAsset(ctx, retry.ID, scan.Asset{Kind: scan.AssetSparsePLY, RelativePath: "output/sparse.ply", SizeBytes: 10})

		// giveUp: 2回着手して2回とも中断された状態を作る（1回目の回復で queued に戻り、もう一度取られる）。
		s.ClaimNext(ctx, base) // giveUp が running（attempts=1）
		if _, _, err := s.RecoverInterrupted(ctx, 2, base.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		// ここで retry と giveUp は両方 queued（attempts=1）。古い順に retry → giveUp と取られる。
		s.ClaimNext(ctx, base.Add(2*time.Minute)) // retry: attempts=2
		s.ClaimNext(ctx, base.Add(2*time.Minute)) // giveUp: attempts=2
		// retry は正常に終わらせ、giveUp だけ running のまま残す。
		s.Finish(ctx, retry.ID, scan.Outcome{Status: scan.StatusSucceeded}, base.Add(3*time.Minute))
		s.UpdateStep(ctx, giveUp.ID, scan.Step{Name: scan.StepSfM, Status: scan.StepRunning, StartedAt: &running})

		requeued, failed, err := s.RecoverInterrupted(ctx, 2, base.Add(4*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(requeued) != 0 || len(failed) != 1 || failed[0] != giveUp.ID {
			t.Fatalf("requeued=%v failed=%v", requeued, failed)
		}

		d := mustGet(t, s, giveUp.ID)
		if d.Scan.Status != scan.StatusFailed || d.Scan.ErrorCode != "interrupted" || d.Scan.ErrorMessage == "" {
			t.Fatalf("上限到達のジョブ: %+v", d.Scan)
		}
		if got := stepStatus(d, scan.StepSfM); got != scan.StepFailed {
			t.Fatalf("実行中だったステップ = %q, want failed", got)
		}
		if got := stepStatus(d, scan.StepSplat); got != scan.StepSkipped {
			t.Fatalf("未着手のステップ = %q, want skipped", got)
		}
		if got := mustGet(t, s, untouched.ID).Scan.Status; got != scan.StatusQueued {
			t.Fatalf("無関係な queued が変わった: %q", got)
		}
		if got := mustGet(t, s, retry.ID).Scan.Status; got != scan.StatusSucceeded {
			t.Fatalf("終了済みが変わった: %q", got)
		}
	})

	t.Run("回復でやり直すジョブは白紙に戻る", func(t *testing.T) {
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)
		s.ClaimNext(ctx, base)
		running := base
		s.UpdateStep(ctx, sc.ID, scan.Step{Name: scan.StepSfM, Status: scan.StepSucceeded, StartedAt: &running, FinishedAt: &running, Message: "ok"})
		s.PutAsset(ctx, sc.ID, scan.Asset{Kind: scan.AssetSparsePLY, RelativePath: "output/sparse.ply", SizeBytes: 10})

		requeued, failed, err := s.RecoverInterrupted(ctx, 2, base.Add(time.Minute))
		if err != nil || len(requeued) != 1 || requeued[0] != sc.ID || len(failed) != 0 {
			t.Fatalf("requeued=%v failed=%v err=%v", requeued, failed, err)
		}
		d := mustGet(t, s, sc.ID)
		if d.Scan.Status != scan.StatusQueued || d.Scan.StartedAt != nil || d.Scan.Attempts != 1 {
			t.Fatalf("やり直し待ちの状態が違う: %+v", d.Scan)
		}
		for _, st := range d.Steps {
			if st.Status != scan.StepPending || st.Message != "" || st.StartedAt != nil {
				t.Fatalf("ステップが初期化されていない: %+v", st)
			}
		}
		if len(d.Assets) != 0 {
			t.Fatalf("成果物が残っている: %v", d.Assets)
		}
	})

	t.Run("期限切れ: 一覧 → expired → 行の削除", func(t *testing.T) {
		s := open(t)
		old, fresh := newScan(t, -48*time.Hour), newScan(t, 0)
		mustCreate(t, s, old)
		mustCreate(t, s, fresh)
		s.PutAsset(ctx, old.ID, scan.Asset{Kind: scan.AssetMeshGLB, RelativePath: "output/mesh.glb", SizeBytes: 10})

		ids, err := s.ListExpired(ctx, base, 10)
		if err != nil || len(ids) != 1 || ids[0] != old.ID {
			t.Fatalf("ListExpired = %v, %v", ids, err)
		}
		if err := s.MarkExpired(ctx, old.ID, base); err != nil {
			t.Fatal(err)
		}
		d := mustGet(t, s, old.ID)
		if d.Scan.Status != scan.StatusExpired || len(d.Assets) != 0 {
			t.Fatalf("expired 後: status=%q assets=%v", d.Scan.Status, d.Assets)
		}
		// expired になったものは二度と一覧に出ない（掃除が同じ行を何度も処理しない）。
		if ids, _ := s.ListExpired(ctx, base, 10); len(ids) != 0 {
			t.Fatalf("expired が再び一覧に出た: %v", ids)
		}

		// 行を残す期間がまだ過ぎていなければ消さない。
		if n, _ := s.PurgeExpired(ctx, old.ExpiresAt.Add(-time.Hour)); n != 0 {
			t.Fatalf("早すぎる削除: %d", n)
		}
		if n, _ := s.PurgeExpired(ctx, old.ExpiresAt.Add(time.Hour)); n != 1 {
			t.Fatalf("PurgeExpired = %d, want 1", n)
		}
		if ok, _ := s.Exists(ctx, old.ID); ok {
			t.Fatal("削除したはずの行が残っている")
		}
		if ok, _ := s.Exists(ctx, fresh.ID); !ok {
			t.Fatal("期限内の行まで消えた")
		}
	})

	t.Run("Delete は関連する行ごと消す", func(t *testing.T) {
		s := open(t)
		sc := newScan(t, 0)
		mustCreate(t, s, sc)
		s.PutAsset(ctx, sc.ID, scan.Asset{Kind: scan.AssetMeshGLB, RelativePath: "output/mesh.glb", SizeBytes: 10})
		if err := s.Delete(ctx, sc.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, sc.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("削除後の Get: %v", err)
		}
	})
}
