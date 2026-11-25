package workerapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ieyoukan/3d-scanner/server/internal/httpapi"
	"github.com/ieyoukan/3d-scanner/server/internal/jobs"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/vps"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

const scanID = "00000000-0000-4000-8000-000000000001"

type fakeJobs struct {
	notified int
	splatErr error
	removed  string
}

func (f *fakeJobs) Notify()                                    { f.notified++ }
func (f *fakeJobs) EnqueueSplat(context.Context, string) error { return f.splatErr }
func (f *fakeJobs) Remove(_ context.Context, id string) (jobs.Removal, error) {
	if id != scanID {
		return "", store.ErrNotFound
	}
	f.removed = id
	return jobs.RemovalCancelled, nil
}

type fakeLocalizer struct {
	gotPath string
	err     error
}

func (f *fakeLocalizer) Localize(_ context.Context, m *workspace.Scan, q vps.Query) (vps.Result, error) {
	f.gotPath = q.Path
	if f.err != nil {
		return vps.Result{}, f.err
	}
	return vps.Result{Localized: true, Position: [3]float64{1, 2, 3}, Forward: [3]float64{0, 0, 1}, Matches: 42, MapImages: 9}, nil
}

// pair は、同じ PVC を共有する API（クライアント）とワーカー（サーバー）を作る。
func pair(t *testing.T) (*Client, *fakeJobs, *fakeLocalizer, *workspace.Root) {
	t.Helper()
	root, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fj, fl := &fakeJobs{}, &fakeLocalizer{}
	info := httpapi.PipelineInfo{MeshMode: "openmvs", SplatDevice: "cpu"}
	srv := httptest.NewServer(NewServer(fj, fl, root, info, nil, nil).Handler())
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, root, httpapi.PipelineInfo{MeshMode: "unknown"}, nil), fj, fl, root
}

func TestClientServerRoundTrip(t *testing.T) {
	c, fj, fl, root := pair(t)
	ctx := context.Background()

	if res, err := c.Remove(ctx, scanID); err != nil || res != jobs.RemovalCancelled || fj.removed != scanID {
		t.Errorf("Remove = %q, %v", res, err)
	}
	if _, err := c.Remove(ctx, "00000000-0000-4000-8000-000000000009"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("無いスキャンの削除 = %v, want ErrNotFound", err)
	}

	fj.splatErr = store.ErrConflict
	if err := c.EnqueueSplat(ctx, scanID); !errors.Is(err, store.ErrConflict) {
		t.Errorf("EnqueueSplat = %v, want ErrConflict", err)
	}

	// 位置合わせ: 写真は共有の一時ディレクトリに置き、相対パスで渡す。
	tmp, _ := root.NewTemp("query-")
	photo := filepath.Join(tmp, "frame_0001.jpg")
	os.WriteFile(photo, []byte("jpeg"), 0o640)
	ws, _ := root.Scan(scanID)
	out, err := c.Localize(ctx, ws, vps.Query{Path: photo, Width: 10, Height: 20})
	if err != nil || !out.Localized || out.Matches != 42 || out.Position != [3]float64{1, 2, 3} {
		t.Errorf("Localize = %+v, %v", out, err)
	}
	if fl.gotPath != photo {
		t.Errorf("ワーカーが受け取ったパス = %q, want %q", fl.gotPath, photo)
	}
	fl.err = vps.ErrBusy
	if _, err := c.Localize(ctx, ws, vps.Query{Path: photo}); !errors.Is(err, vps.ErrBusy) {
		t.Errorf("Localize（処理中）= %v, want ErrBusy", err)
	}
	// 一時ディレクトリの外の写真は、送る前に断る。
	if _, err := c.Localize(ctx, ws, vps.Query{Path: "/etc/passwd"}); err == nil {
		t.Error("一時ディレクトリの外のパスを送った")
	}

	if got := c.PipelineInfo(ctx); got.MeshMode != "openmvs" {
		t.Errorf("PipelineInfo = %+v", got)
	}
}

func TestServerRejectsPathOutsideTemp(t *testing.T) {
	root, _ := workspace.New(t.TempDir())
	srv := httptest.NewServer(NewServer(&fakeJobs{}, &fakeLocalizer{}, root, httpapi.PipelineInfo{}, nil, nil).Handler())
	defer srv.Close()
	c := NewClient(srv.URL, root, httpapi.PipelineInfo{}, nil)
	var out localizeResult
	for _, p := range []string{"../scans/x/images/frame_0001.jpg", "/etc/passwd", ""} {
		err := c.do(context.Background(), "POST", "/internal/scans/"+scanID+"/localize", localizeRequest{Path: p}, &out)
		if err == nil {
			t.Errorf("パス %q を受け付けた", p)
		}
	}
}

func TestClientWorkerDown(t *testing.T) {
	root, _ := workspace.New(t.TempDir())
	// 誰も待ち受けていない宛先。
	c := NewClient("http://127.0.0.1:1", root, httpapi.PipelineInfo{MeshMode: "fallback"}, nil)
	if _, err := c.Remove(context.Background(), scanID); !errors.Is(err, httpapi.ErrWorkerUnavailable) {
		t.Errorf("Remove = %v, want ErrWorkerUnavailable", err)
	}
	if got := c.PipelineInfo(context.Background()); got.MeshMode != "fallback" {
		t.Errorf("届かないときは設定から分かる値を返すはず: %+v", got)
	}
}
