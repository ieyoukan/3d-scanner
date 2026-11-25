package reconstruction

import (
	"fmt"
	"math"
	"testing"
)

func frameNames(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("frame_%04d.jpg", i))
	}
	return out
}

func hasPair(pairs [][2]string, a, b string) bool {
	for _, p := range pairs {
		if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
			return true
		}
	}
	return false
}

func TestScenePairsSequentialAndLoop(t *testing.T) {
	names := frameNames(1, 150)
	pairs := ScenePairs(names, nil, nil)
	if !hasPair(pairs, "frame_0001.jpg", "frame_0009.jpg") || hasPair(pairs, "frame_0001.jpg", "frame_0010.jpg") {
		t.Error("前後 8 枚だけを組にするはず")
	}
	if !hasPair(pairs, "frame_0001.jpg", "frame_0129.jpg") {
		t.Error("128 枚離れた写真（1周して戻ってきた所）とも組にするはず")
	}
	// 全部の組（11175）よりずっと少ない。
	if len(pairs) > 150*(sceneOverlap+len(sceneLongJumps)) || len(pairs) < 1000 {
		t.Errorf("組の数 = %d", len(pairs))
	}
	seen := map[[2]string]bool{}
	for _, p := range pairs {
		if p[0] >= p[1] || seen[p] {
			t.Fatalf("組が重複しているか、順番が逆: %v", p)
		}
		seen[p] = true
	}
}

func TestScenePairsARNeighbors(t *testing.T) {
	names := frameNames(1, 60)
	// 1〜30 枚目で廊下を往き、31〜60 枚目で同じ所を戻る（逆向き）。40 枚目は 20 枚目と同じ位置で同じ向き。
	face := func(yawDeg float64) [4]float64 {
		h := yawDeg * math.Pi / 360
		return [4]float64{0, math.Sin(h), 0, math.Cos(h)}
	}
	var ar []ARPose
	for i := 1; i <= 60; i++ {
		x := float64(i-1) * 0.2
		o := face(0)
		if i > 30 {
			x = float64(60-i) * 0.2
			o = face(180)
		}
		ar = append(ar, ARPose{Name: names[i-1], Session: "s", Position: [3]float64{x, 1.5, 0}, Orientation: o})
	}
	ar[39].Orientation = face(0) // 40 枚目だけ往きと同じ向き
	pairs := ScenePairs(names, nil, ar)
	if !hasPair(pairs, "frame_0040.jpg", "frame_0021.jpg") {
		t.Error("同じ所を同じ向きで撮った写真（AR の位置が近い）を組にするはず")
	}
	if hasPair(pairs, "frame_0041.jpg", "frame_0019.jpg") {
		t.Error("同じ所でも反対を向いた写真は組にしないはず")
	}
}

func TestScenePairsResume(t *testing.T) {
	names := frameNames(1, 100)
	inherited := map[string]bool{}
	for _, n := range names[:80] {
		inherited[n] = true
	}
	pairs := ScenePairs(names, inherited, nil)
	for _, p := range pairs {
		if inherited[p[0]] && inherited[p[1]] {
			t.Fatalf("引き継いだ写真どうしは照合済みなので組にしない: %v", p)
		}
	}
	if !hasPair(pairs, "frame_0081.jpg", "frame_0001.jpg") {
		t.Error("今回の写真は、引き継いだ写真（等間隔に選んだもの）と組にするはず")
	}
}

func TestUnregisteredRuns(t *testing.T) {
	all := frameNames(1, 60)
	var registered []string
	for i, n := range all {
		if (i+1 >= 41 && i+1 <= 55) || i+1 == 7 {
			continue
		}
		registered = append(registered, n)
	}
	runs := UnregisteredRuns(all, registered)
	if len(runs) != 2 || runs[0] != [2]int{7, 7} || runs[1] != [2]int{41, 55} {
		t.Errorf("runs = %v", runs)
	}
	if got := describeRuns(runs); got != "7、41〜55枚目" {
		t.Errorf("describeRuns = %q", got)
	}
}

func TestParseCaptureMode(t *testing.T) {
	for in, want := range map[string]CaptureMode{"": ModeObject, "object": ModeObject, "scene": ModeScene} {
		if got, ok := ParseCaptureMode(in); !ok || got != want {
			t.Errorf("%q → %q, %v", in, got, ok)
		}
	}
	if _, ok := ParseCaptureMode("room"); ok {
		t.Error("知らない値を受け付けた")
	}
}
