package jobs

import (
	"context"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// orphanGrace は「DB に行が無い作業ディレクトリ」を消すまでの猶予。
//
// アップロードの受付は「ディレクトリを作って写真を保存 → DB に行を作る」の順で進む。
// その途中のディレクトリは、DB から見るとまだ存在しないスキャンのものに見える。
// 掃除がそれを消してしまわないよう、作られてから十分時間が経ったものだけを対象にする。
const orphanGrace = time.Hour

// SweepResult は掃除1回ぶんの結果。
type SweepResult struct {
	Expired int // 期限切れにして、ファイルを消した件数
	Purged  int // 期限切れのまま十分経ったので、行も消した件数
	Orphans int // DB に行が無いので消したディレクトリの数
	// KeptOrphans は、DB に行が無いが保存したスキャンの目印があるので、消さずに残したディレクトリの数。
	KeptOrphans int
}

// RunJanitor は掃除を定期的に実行する。ctx が終わるまで戻らない。
func (s *Service) RunJanitor(ctx context.Context) {
	// 起動直後にも1回実行する（停止している間に期限が過ぎたものをすぐ片付ける）。
	for {
		if res, err := s.Sweep(ctx); err != nil {
			s.log.Error("掃除に失敗", "error", err)
		} else if res != (SweepResult{}) {
			s.log.Info("掃除を実行", "expired", res.Expired, "purged", res.Purged, "orphans", res.Orphans)
			if res.KeptOrphans > 0 {
				s.log.Warn("DB に行が無い保存済みスキャンのディレクトリを、消さずに残しました", "count", res.KeptOrphans)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.JanitorInterval):
		}
	}
}

// Sweep は掃除を1回行う。
//
// MVP は「長期保存しない」方針なので、保存期限（既定24時間）を過ぎたスキャンの
// 写真・中間ファイル・成果物を消す。プライバシー（写真を残し続けない）と
// ディスク容量（PVC は有限）の両方のため。
//
// 副作用: ファイルの削除と DB の更新。
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	now := s.now()

	// (1) 期限切れ: ファイルを消してから状態を expired にする。
	// この順番なら、途中で落ちても status は expired になっていないので次回もう一度処理される
	// （逆順だと「expired なのにファイルが残っている」状態が二度と掃除されない）。
	ids, err := s.store.ListExpired(ctx, now, 100)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		ws, err := s.root.Scan(id)
		if err != nil {
			continue
		}
		// まだ処理待ち・処理中なら中止する（期限を過ぎたジョブは、結果を見る人がもう居ない）。
		// 利用者のキャンセルと同じ手順: 先に DB を cancelled にしてから、実行中のプロセスを止める。
		prev, changed, err := s.store.Cancel(ctx, id, now)
		if err != nil {
			s.log.Error("期限切れのジョブを中止できません", "scan", id, "error", err)
			continue
		}
		if changed && prev == scan.StatusRunning && s.cancelRunning(id) {
			// 外部プロセスが止まるまでファイルは消せない（消すのはワーカー）。
			// 状態は cancelled になったので、次回の掃除で expired に進む。
			continue
		}
		// あとで作る 3DGS を学習中なら止める（保存しない設定のとき、完成したスキャンも期限で消える）。
		s.stopSplat(id)
		if err := ws.Remove(); err != nil {
			s.log.Error("期限切れのファイルを削除できません", "scan", id, "error", err)
			continue
		}
		if err := s.store.MarkExpired(ctx, id, now); err != nil {
			s.log.Error("期限切れを記録できません", "scan", id, "error", err)
			continue
		}
		res.Expired++
	}

	// (2) 「期限切れ」と表示するために残していた行も、十分経ったら消す。
	n, err := s.store.PurgeExpired(ctx, now.Add(-s.cfg.ExpiredRowRetention))
	if err != nil {
		return res, err
	}
	res.Purged = n

	// (3) 孤児ディレクトリ: DB に行が無いのに残っている作業ディレクトリ。
	// 受付の途中でサーバーが落ちた場合などに出来る。
	dirs, err := s.root.List()
	if err != nil {
		return res, err
	}
	for _, id := range dirs {
		exists, err := s.store.Exists(ctx, id)
		if err != nil {
			return res, err
		}
		if exists {
			continue
		}
		ws, err := s.root.Scan(id)
		if err != nil {
			continue
		}
		modified, err := ws.ModTime()
		if err != nil || now.Sub(modified) < orphanGrace {
			continue
		}
		// 保存した 3D データは、DB に行が無くても消さない（DB を作り直した・戻した場合に備える）。
		if ws.IsSaved() {
			res.KeptOrphans++
			continue
		}
		if err := ws.Remove(); err != nil {
			s.log.Error("孤児ディレクトリを削除できません", "scan", id, "error", err)
			continue
		}
		res.Orphans++
	}
	return res, nil
}
