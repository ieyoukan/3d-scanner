// Package store はスキャンの状態を永続化する（リポジトリ層）。
//
// Store インターフェースを挟む理由は1つだけ: HTTP ハンドラやワーカーのテストを
// PostgreSQL 無しで走らせるため。実装は2つある。
//   - Postgres: 本番用。
//   - Memory:   テスト用。Postgres と同じ決まりで動く（contract_test.go が両方に同じテストを流す）。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// ErrNotFound は指定した ID のスキャンが無いことを表す。
var ErrNotFound = errors.New("スキャンが見つかりません")

// Store はスキャンの保存と状態遷移を提供する。
//
// 時刻（now）を引数で受け取るメソッドが多いのは、テストで時間を自由に進められるようにするため。
// 状態遷移は必ず「いまの状態が期待どおりのときだけ更新する」形にしてある
// （例: Finish は running の行しか更新しない）。こうしておくと、利用者のキャンセルと
// ワーカーの完了がほぼ同時に起きても、後から来た方が先の結果を上書きしない。
type Store interface {
	// Ping は DB に接続できるかを確かめる（/readyz 用）。
	Ping(ctx context.Context) error

	// Create はスキャンを queued で登録する。
	// 本体・写真の記録・全ステップ（pending）を1つのトランザクションで作る。
	// 途中で失敗したら何も残らない（半端な行があると待ち行列に幽霊ジョブが残る）。
	Create(ctx context.Context, s scan.Scan, frames []scan.Frame) error

	// Get はスキャンの全体像を返す。無ければ ErrNotFound。
	Get(ctx context.Context, id string) (scan.Detail, error)

	// CountActive は処理待ち・処理中のスキャン数を返す（受付上限の判定用）。
	CountActive(ctx context.Context) (int, error)

	// QueuePosition は、指定したスキャンより前に並んでいる queued の件数を返す。
	QueuePosition(ctx context.Context, id string) (int, error)

	// ClaimNext は待ち行列の先頭（最も古い queued）を running にして返す。
	// 待ちが無ければ (nil, nil)。「選ぶ」と「running にする」を1文で行うので、
	// 同じジョブを2つのワーカーが同時に取ることはない（ジョブの排他）。
	ClaimNext(ctx context.Context, now time.Time) (*scan.Scan, error)

	// UpdateStep はステップ1件の状態を上書きする。
	UpdateStep(ctx context.Context, scanID string, st scan.Step) error

	// PutAsset は検証済みの成果物を登録する（同じ種類があれば置き換える）。
	PutAsset(ctx context.Context, scanID string, a scan.Asset) error

	// Finish は running のスキャンを終了状態にする。
	// 戻り値 false は「running ではなかった（＝キャンセル済みなど）ので何もしなかった」。
	Finish(ctx context.Context, id string, out scan.Outcome, now time.Time) (bool, error)

	// Cancel は queued / running のスキャンを cancelled にする。
	// prev は直前の状態。changed が false なら既に終了していて何も変えていない。
	Cancel(ctx context.Context, id string, now time.Time) (prev scan.Status, changed bool, err error)

	// Delete はスキャンの行を消す（関連行は ON DELETE CASCADE で一緒に消える）。
	Delete(ctx context.Context, id string) error

	// Exists は行があるかを返す（孤児ディレクトリの掃除用）。
	Exists(ctx context.Context, id string) (bool, error)

	// RecoverInterrupted は起動時に呼ぶ。running のまま残っている行は、前回のプロセスが
	// 処理の途中で終了した（再起動・クラッシュ）ことを意味する。
	//   - 着手回数が maxAttempts 未満: queued に戻してやり直す（ステップと成果物は初期化）。
	//   - それ以上: failed にする（同じジョブがサーバーを落とし続けるのを止める）。
	// 前提: ワーカーは1プロセスだけ。複数いると「他のワーカーが実行中の行」まで戻してしまう。
	RecoverInterrupted(ctx context.Context, maxAttempts int, now time.Time) (requeued, failed []string, err error)

	// ListExpired は保存期限を過ぎ、まだ expired になっていないスキャンの ID を返す。
	ListExpired(ctx context.Context, now time.Time, limit int) ([]string, error)

	// MarkExpired はスキャンを expired にし、成果物の目録を消す（ファイルの削除は呼び出し側）。
	MarkExpired(ctx context.Context, id string, now time.Time) error

	// PurgeExpired は expired になってから十分経った行を消し、消した件数を返す。
	PurgeExpired(ctx context.Context, expiredBefore time.Time) (int, error)

	// Close は接続を閉じる。
	Close() error
}

// 中断・再開時に画面へ出す文言。Postgres と Memory の両実装で同じものを使う。
const (
	interruptedCode    = "interrupted"
	interruptedMessage = "サーバーの再起動で処理が中断され、やり直しの上限に達しました。もう一度お試しください。"
	interruptedStep    = "サーバーの再起動で中断されました。"
	notRunStep         = "中断されたため実行していません。"
)
