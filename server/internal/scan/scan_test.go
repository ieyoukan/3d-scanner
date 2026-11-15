package scan

import "testing"

// step はテスト用に Step を短く書くための補助。
func step(name StepName, status StepStatus) Step {
	s := Step{Name: name, Status: status}
	if status == StepFailed {
		s.ErrorMessage = string(name) + " が失敗"
	}
	return s
}

// TestSummarize は「全体の状態の決め方」を表にして確かめる。
// 指示書の完了条件6（失敗が成功表示にならない／部分成功を扱える）を守るための中心のテスト。
func TestSummarize(t *testing.T) {
	cases := []struct {
		name     string
		steps    []Step
		assets   []AssetKind
		want     Status
		wantCode string
	}{
		{
			name: "全部成功",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSucceeded), step(StepMesh, StepSucceeded),
				step(StepSplat, StepSucceeded), step(StepSOG, StepSucceeded)},
			assets: []AssetKind{AssetSparsePLY, AssetMeshGLB, AssetGaussianPLY, AssetGaussianSOG},
			want:   StatusSucceeded,
		},
		{
			name: "MVSはスキップ(GPUなし)でも他が成功なら成功",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSkipped), step(StepMesh, StepSucceeded),
				step(StepSplat, StepSucceeded), step(StepSOG, StepSucceeded)},
			assets: []AssetKind{AssetSparsePLY, AssetMeshGLB, AssetGaussianPLY, AssetGaussianSOG},
			want:   StatusSucceeded,
		},
		{
			name: "Meshだけ成功(3DGS失敗)は部分成功",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSkipped), step(StepMesh, StepSucceeded),
				step(StepSplat, StepFailed), step(StepSOG, StepSkipped)},
			assets:   []AssetKind{AssetSparsePLY, AssetMeshGLB},
			want:     StatusPartial,
			wantCode: "splat_failed",
		},
		{
			name: "3DGSだけ成功(Mesh失敗)は部分成功",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepFailed), step(StepMesh, StepSkipped),
				step(StepSplat, StepSucceeded), step(StepSOG, StepSucceeded)},
			assets:   []AssetKind{AssetSparsePLY, AssetGaussianPLY, AssetGaussianSOG},
			want:     StatusPartial,
			wantCode: "mvs_failed",
		},
		{
			name: "SOG変換だけ失敗してもPLYで見られるので部分成功",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSkipped), step(StepMesh, StepSucceeded),
				step(StepSplat, StepSucceeded), step(StepSOG, StepFailed)},
			assets:   []AssetKind{AssetSparsePLY, AssetMeshGLB, AssetGaussianPLY},
			want:     StatusPartial,
			wantCode: "sog_failed",
		},
		{
			name: "SfM失敗は失敗",
			steps: []Step{step(StepSfM, StepFailed), step(StepMVS, StepSkipped), step(StepMesh, StepSkipped),
				step(StepSplat, StepSkipped), step(StepSOG, StepSkipped)},
			assets:   nil,
			want:     StatusFailed,
			wantCode: "sfm_failed",
		},
		{
			name: "疎な点群だけでは完成と呼ばない(両分岐が失敗)",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSkipped), step(StepMesh, StepFailed),
				step(StepSplat, StepFailed), step(StepSOG, StepSkipped)},
			assets:   []AssetKind{AssetSparsePLY},
			want:     StatusFailed,
			wantCode: "mesh_failed",
		},
		{
			name: "失敗は無いが見られる成果物も無い(両分岐を設定で無効)は失敗",
			steps: []Step{step(StepSfM, StepSucceeded), step(StepMVS, StepSkipped), step(StepMesh, StepSkipped),
				step(StepSplat, StepSkipped), step(StepSOG, StepSkipped)},
			assets:   []AssetKind{AssetSparsePLY},
			want:     StatusFailed,
			wantCode: "no_output",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Summarize(c.steps, c.assets)
			if got.Status != c.want {
				t.Fatalf("status = %q, want %q", got.Status, c.want)
			}
			if got.ErrorCode != c.wantCode {
				t.Fatalf("errorCode = %q, want %q", got.ErrorCode, c.wantCode)
			}
			if got.Status != StatusSucceeded && got.ErrorMessage == "" {
				t.Fatalf("成功以外では画面に出す説明が必要")
			}
		})
	}
}

func TestStatusPredicates(t *testing.T) {
	for _, s := range []Status{StatusQueued, StatusRunning} {
		if s.Terminal() || !s.Active() {
			t.Errorf("%s は処理中の状態のはず", s)
		}
	}
	for _, s := range []Status{StatusPartial, StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired} {
		if !s.Terminal() || s.Active() {
			t.Errorf("%s は終了状態のはず", s)
		}
	}
}

func TestNewIDAndValidID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidID(id) {
			t.Fatalf("NewID が ValidID を通らない: %q", id)
		}
		if seen[id] {
			t.Fatalf("ID が重複した: %q", id)
		}
		seen[id] = true
	}

	// パスに意味を持つ文字や長さ違いは必ず拒否する。
	bad := []string{
		"", "abc", "../../etc/passwd",
		"00000000-0000-4000-8000-00000000000",   // 1文字短い
		"00000000-0000-4000-8000-0000000000000", // 1文字長い
		"00000000-0000-4000-8000-00000000000G",  // 16進でない
		"00000000-0000-4000-8000-00000000000A",  // 大文字は NewID が作らない
		"00000000/0000-4000-8000-000000000000",
		"00000000-0000-4000-8000-0000000000.." + "",
	}
	for _, s := range bad {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true, want false", s)
		}
	}
}
