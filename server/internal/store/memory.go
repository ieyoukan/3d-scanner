package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// Memory はメモリ上だけで動く Store。テスト専用（プロセスを止めると全部消える）。
//
// Postgres と同じ決まりで動くように書いてある。両者が食い違わないことは
// contract_test.go が同じテストを両方に流して確かめる。
type Memory struct {
	mu    sync.Mutex
	scans map[string]*memScan
}

type memScan struct {
	scan   scan.Scan
	frames []scan.Frame
	steps  []scan.Step
	assets map[scan.AssetKind]scan.Asset
	// splat は 3DGS の待ち行列の行（無ければ nil）。行と一緒に消えるので ON DELETE CASCADE と同じ。
	splat *scan.SplatJob
}

// NewMemory は空の Memory を返す。
func NewMemory() *Memory {
	return &Memory{scans: map[string]*memScan{}}
}

func (m *Memory) Close() error               { return nil }
func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Create(_ context.Context, s scan.Scan, frames []scan.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Status = scan.StatusQueued
	s.UpdatedAt = s.CreatedAt
	ms := &memScan{scan: s, frames: append([]scan.Frame(nil), frames...), assets: map[scan.AssetKind]scan.Asset{}}
	for _, name := range scan.StepOrder {
		ms.steps = append(ms.steps, scan.Step{Name: name, Status: scan.StepPending})
	}
	m.scans[s.ID] = ms
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (scan.Detail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok {
		return scan.Detail{}, ErrNotFound
	}
	d := scan.Detail{Scan: ms.scan, Steps: append([]scan.Step(nil), ms.steps...)}
	if ms.splat != nil {
		j := *ms.splat
		d.Splat = &j
	}
	for _, a := range ms.assets {
		d.Assets = append(d.Assets, a)
	}
	sort.Slice(d.Assets, func(i, j int) bool { return d.Assets[i].Kind < d.Assets[j].Kind })
	return d, nil
}

func (m *Memory) CountActive(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ms := range m.scans {
		if ms.scan.Status.Active() {
			n++
		}
	}
	return n, nil
}

// before は待ち行列の並び順（作成時刻、同時刻なら ID）で a が b より前かを返す。
func before(a, b scan.Scan) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func (m *Memory) QueuePosition(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	me, ok := m.scans[id]
	if !ok {
		return 0, nil
	}
	n := 0
	for _, ms := range m.scans {
		if ms.scan.Status == scan.StatusQueued && before(ms.scan, me.scan) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) ClaimNext(_ context.Context, now time.Time) (*scan.Scan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next *memScan
	for _, ms := range m.scans {
		if ms.scan.Status != scan.StatusQueued {
			continue
		}
		if next == nil || before(ms.scan, next.scan) {
			next = ms
		}
	}
	if next == nil {
		return nil, nil
	}
	t := now
	next.scan.Status = scan.StatusRunning
	next.scan.StartedAt = &t
	next.scan.UpdatedAt = now
	next.scan.Attempts++
	s := next.scan
	return &s, nil
}

func (m *Memory) UpdateStep(_ context.Context, scanID string, st scan.Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[scanID]
	if !ok {
		return ErrNotFound
	}
	for i := range ms.steps {
		if ms.steps[i].Name == st.Name {
			ms.steps[i] = st
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) PutAsset(_ context.Context, scanID string, a scan.Asset) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[scanID]
	if !ok {
		return ErrNotFound
	}
	ms.assets[a.Kind] = a
	return nil
}

func (m *Memory) List(_ context.Context, q ListQuery) ([]scan.Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// newer は一覧の並び（作成時刻の新しい順、同時刻なら ID の降順）で a が b より前かを返す。
	newer := func(a, b scan.Scan) bool {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	}
	// superseded は「続きの版が完成している」古い版。一覧には出さない。
	superseded := map[string]bool{}
	for _, ms := range m.scans {
		if ms.scan.ParentID != "" && ms.scan.Status.Keepable() {
			superseded[ms.scan.ParentID] = true
		}
	}
	var hit []*memScan
	for _, ms := range m.scans {
		if ms.scan.Status == scan.StatusExpired || superseded[ms.scan.ID] {
			continue
		}
		if q.After != nil && !newer(scan.Scan{CreatedAt: q.After.CreatedAt, ID: q.After.ID}, ms.scan) {
			continue
		}
		hit = append(hit, ms)
	}
	sort.Slice(hit, func(i, j int) bool { return newer(hit[i].scan, hit[j].scan) })
	var out []scan.Summary
	for i, ms := range hit {
		if i >= q.Limit {
			break
		}
		sum := scan.Summary{Scan: ms.scan, SplatPending: ms.splat != nil}
		for k := range ms.assets {
			sum.AssetKinds = append(sum.AssetKinds, k)
		}
		sort.Slice(sum.AssetKinds, func(i, j int) bool { return sum.AssetKinds[i] < sum.AssetKinds[j] })
		out = append(out, sum)
	}
	return out, nil
}

func (m *Memory) Children(_ context.Context, id string) ([]scan.Scan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []scan.Scan
	for _, ms := range m.scans {
		if ms.scan.ParentID == id && ms.scan.Status != scan.StatusExpired {
			out = append(out, ms.scan)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// detachChildren は、消えるスキャンを指している続きの版の ParentID を空にする
// （Postgres の ON DELETE SET NULL と同じ振る舞い）。m.mu を持った状態で呼ぶ。
func (m *Memory) detachChildren(id string) {
	for _, ms := range m.scans {
		if ms.scan.ParentID == id {
			ms.scan.ParentID = ""
		}
	}
}

func (m *Memory) SetTitle(_ context.Context, id, title string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok || ms.scan.Status == scan.StatusExpired {
		return ErrNotFound
	}
	ms.scan.Title = title
	ms.scan.UpdatedAt = now
	return nil
}

func (m *Memory) Finish(_ context.Context, id string, out scan.Outcome, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok || ms.scan.Status != scan.StatusRunning {
		return false, nil
	}
	t := now
	ms.scan.Status = out.Status
	ms.scan.Saved = out.Saved
	ms.scan.ErrorCode, ms.scan.ErrorMessage = out.ErrorCode, out.ErrorMessage
	ms.scan.FinishedAt, ms.scan.UpdatedAt = &t, now
	return true, nil
}

func (m *Memory) Cancel(_ context.Context, id string, now time.Time) (scan.Status, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok {
		return "", false, ErrNotFound
	}
	prev := ms.scan.Status
	if !prev.Active() {
		return prev, false, nil
	}
	t := now
	ms.scan.Status = scan.StatusCancelled
	ms.scan.ErrorCode, ms.scan.ErrorMessage = "cancelled", "利用者が中止しました。"
	ms.scan.FinishedAt, ms.scan.UpdatedAt = &t, now
	for i := range ms.steps {
		if ms.steps[i].Status == scan.StepPending || ms.steps[i].Status == scan.StepRunning {
			ms.steps[i].Status = scan.StepSkipped
			ms.steps[i].FinishedAt = &t
			ms.steps[i].Message = "中止されました。"
		}
	}
	ms.assets = map[scan.AssetKind]scan.Asset{}
	return prev, true, nil
}

func (m *Memory) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.scans[id]; !ok {
		return ErrNotFound
	}
	delete(m.scans, id)
	m.detachChildren(id)
	return nil
}

func (m *Memory) Exists(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.scans[id]
	return ok, nil
}

func (m *Memory) RecoverInterrupted(_ context.Context, maxAttempts int, now time.Time) (requeued, failed []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := now
	for id, ms := range m.scans {
		if ms.scan.Status != scan.StatusRunning {
			continue
		}
		if ms.scan.Attempts >= maxAttempts {
			ms.scan.Status = scan.StatusFailed
			ms.scan.ErrorCode, ms.scan.ErrorMessage = interruptedCode, interruptedMessage
			ms.scan.FinishedAt, ms.scan.UpdatedAt = &t, now
			for i := range ms.steps {
				switch ms.steps[i].Status {
				case scan.StepRunning:
					ms.steps[i].Status, ms.steps[i].ErrorMessage, ms.steps[i].FinishedAt = scan.StepFailed, interruptedStep, &t
				case scan.StepPending:
					ms.steps[i].Status, ms.steps[i].Message, ms.steps[i].FinishedAt = scan.StepSkipped, notRunStep, &t
				}
			}
			failed = append(failed, id)
			continue
		}
		ms.scan.Status = scan.StatusQueued
		ms.scan.StartedAt = nil
		ms.scan.UpdatedAt = now
		for i := range ms.steps {
			ms.steps[i] = scan.Step{Name: ms.steps[i].Name, Status: scan.StepPending}
		}
		ms.assets = map[scan.AssetKind]scan.Asset{}
		requeued = append(requeued, id)
	}
	sort.Strings(requeued)
	sort.Strings(failed)
	return requeued, failed, nil
}

func (m *Memory) ListExpired(_ context.Context, now time.Time, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hit []scan.Scan
	for _, ms := range m.scans {
		if ms.scan.ExpiresAt.Before(now) && ms.scan.Status != scan.StatusExpired && !ms.scan.Saved {
			hit = append(hit, ms.scan)
		}
	}
	sort.Slice(hit, func(i, j int) bool { return hit[i].ExpiresAt.Before(hit[j].ExpiresAt) })
	var ids []string
	for i, s := range hit {
		if i >= limit {
			break
		}
		ids = append(ids, s.ID)
	}
	return ids, nil
}

func (m *Memory) MarkExpired(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok {
		return ErrNotFound
	}
	ms.scan.Status = scan.StatusExpired
	ms.scan.UpdatedAt = now
	if ms.scan.FinishedAt == nil {
		t := now
		ms.scan.FinishedAt = &t
	}
	ms.assets = map[scan.AssetKind]scan.Asset{}
	ms.splat = nil
	return nil
}

// ---- あとで作る 3DGS の待ち行列 ----

// resetSplatSteps は splat / sog のステップを「待ち」に戻す。m.mu を持った状態で呼ぶ。
func (ms *memScan) resetSplatSteps(message string) {
	for i := range ms.steps {
		if ms.steps[i].Name == scan.StepSplat || ms.steps[i].Name == scan.StepSOG {
			ms.steps[i] = scan.Step{Name: ms.steps[i].Name, Status: scan.StepPending, Message: message}
		}
	}
}

func (m *Memory) EnqueueSplat(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok {
		return ErrNotFound
	}
	if !ms.scan.Status.Keepable() || ms.splat != nil {
		return ErrConflict
	}
	ms.splat = &scan.SplatJob{Status: scan.SplatQueued, QueuedAt: now}
	ms.resetSplatSteps(splatQueuedStep)
	return nil
}

func (m *Memory) ClaimNextSplat(_ context.Context, now time.Time) (*scan.Scan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next *memScan
	for _, ms := range m.scans {
		if ms.splat == nil || ms.splat.Status != scan.SplatQueued || !ms.scan.Status.Keepable() {
			continue
		}
		if next == nil || ms.splat.QueuedAt.Before(next.splat.QueuedAt) ||
			(ms.splat.QueuedAt.Equal(next.splat.QueuedAt) && ms.scan.ID < next.scan.ID) {
			next = ms
		}
	}
	if next == nil {
		return nil, nil
	}
	t := now
	next.splat.Status, next.splat.StartedAt = scan.SplatRunning, &t
	next.splat.Attempts++
	next.resetSplatSteps("")
	sc := next.scan
	return &sc, nil
}

func (m *Memory) FinishSplat(_ context.Context, id string, out scan.Outcome, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok || ms.splat == nil || ms.splat.Status != scan.SplatRunning {
		return false, nil
	}
	ms.splat = nil
	if ms.scan.Status.Keepable() {
		ms.scan.Status, ms.scan.ErrorCode, ms.scan.ErrorMessage = out.Status, out.ErrorCode, out.ErrorMessage
		ms.scan.UpdatedAt = now
	}
	return true, nil
}

func (m *Memory) RequeueSplat(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.scans[id]
	if !ok || ms.splat == nil || ms.splat.Status != scan.SplatRunning {
		return ErrNotFound
	}
	ms.splat.Status, ms.splat.StartedAt = scan.SplatQueued, nil
	ms.splat.Attempts = max(ms.splat.Attempts-1, 0)
	ms.resetSplatSteps(splatYieldedStep)
	return nil
}

func (m *Memory) SplatQueuePosition(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	me, ok := m.scans[id]
	if !ok || me.splat == nil {
		return 0, nil
	}
	n := 0
	for _, ms := range m.scans {
		if ms.splat == nil || ms.splat.Status != scan.SplatQueued {
			continue
		}
		a, b := ms.splat.QueuedAt, me.splat.QueuedAt
		if a.Before(b) || (a.Equal(b) && ms.scan.ID < me.scan.ID) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) RecoverSplats(_ context.Context, maxAttempts int, now time.Time) (requeued, failed []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := now
	for id, ms := range m.scans {
		if ms.splat == nil || ms.splat.Status != scan.SplatRunning {
			continue
		}
		if ms.splat.Attempts >= maxAttempts {
			ms.splat = nil
			for i := range ms.steps {
				switch {
				case ms.steps[i].Name == scan.StepSplat:
					ms.steps[i].Status, ms.steps[i].FinishedAt, ms.steps[i].ErrorMessage = scan.StepFailed, &t, interruptedStep
				case ms.steps[i].Name == scan.StepSOG && (ms.steps[i].Status == scan.StepPending || ms.steps[i].Status == scan.StepRunning):
					ms.steps[i].Status, ms.steps[i].FinishedAt, ms.steps[i].Message = scan.StepSkipped, &t, notRunStep
				}
			}
			if ms.scan.Status.Keepable() {
				ms.scan.Status, ms.scan.ErrorCode, ms.scan.ErrorMessage = scan.StatusPartial, splatFailedCode, splatFailedMessage
				ms.scan.UpdatedAt = now
			}
			failed = append(failed, id)
			continue
		}
		ms.splat.Status, ms.splat.StartedAt = scan.SplatQueued, nil
		ms.resetSplatSteps(splatQueuedStep)
		requeued = append(requeued, id)
	}
	return requeued, failed, nil
}

func (m *Memory) PurgeExpired(_ context.Context, expiredBefore time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, ms := range m.scans {
		if ms.scan.Status == scan.StatusExpired && ms.scan.ExpiresAt.Before(expiredBefore) {
			delete(m.scans, id)
			m.detachChildren(id)
			n++
		}
	}
	return n, nil
}
