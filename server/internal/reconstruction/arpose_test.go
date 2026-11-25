package reconstruction

import (
	"fmt"
	"math"
	"testing"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// orbitPair は、SfM の座標で半径 r の円を回るカメラと、それを既知の s, R, t で AR の座標に移したものを作る。
func orbitPair(n int, r float64, s float64, R [3][3]float64, t [3]float64, session string) ([]ImagePose, []ARPose) {
	var poses []ImagePose
	var ar []ARPose
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		c := [3]float64{r * math.Cos(a), 0.3 * math.Sin(3*a), r * math.Sin(a)}
		name := fmt.Sprintf("frame_%04d.jpg", i+1)
		poses = append(poses, ImagePose{Name: name, Center: c})
		p := mulMat3(R, c)
		ar = append(ar, ARPose{Name: name, Session: session, Position: [3]float64{s*p[0] + t[0], s*p[1] + t[1], s*p[2] + t[2]}})
	}
	return poses, ar
}

func TestAlignToARRecoversScaleAndUp(t *testing.T) {
	// SfM の座標は AR に対して、軸 (1,2,3) のまわりに 70° 回っていて、1 が 0.25 m。
	ax, _ := normalize([3]float64{1, 2, 3})
	h := 70 * math.Pi / 180 / 2
	R := quatToMatrix(math.Cos(h), ax[0]*math.Sin(h), ax[1]*math.Sin(h), ax[2]*math.Sin(h))
	poses, ar := orbitPair(24, 3, 0.25, R, [3]float64{0.4, 1.2, -0.7}, "a")

	al, ok := AlignToAR(poses, ar)
	if !ok {
		t.Fatal("合わせられなかった")
	}
	if math.Abs(al.Scale-0.25) > 1e-6 {
		t.Errorf("縮尺 = %v, want 0.25", al.Scale)
	}
	// 真上（SfM の座標）を R で AR に移すと (0,1,0) になる。
	up := mulMat3(R, al.Up)
	if math.Abs(up[1]-1) > 1e-6 {
		t.Errorf("真上を AR に移すと %v", up)
	}
	if al.RMS > 1e-6 || al.Images != 24 {
		t.Errorf("RMS = %v, images = %d", al.RMS, al.Images)
	}
}

func TestAlignToARRejects(t *testing.T) {
	id := [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

	t.Run("枚数が足りない", func(t *testing.T) {
		poses, ar := orbitPair(4, 1, 1, id, [3]float64{}, "a")
		if _, ok := AlignToAR(poses, ar); ok {
			t.Error("4枚で合わせた")
		}
	})

	t.Run("ほとんど動いていない（その場で回っただけ）", func(t *testing.T) {
		poses, ar := orbitPair(20, 1, 0.01, id, [3]float64{}, "a")
		if _, ok := AlignToAR(poses, ar); ok {
			t.Error("1cm しか動いていないのに合わせた")
		}
	})

	t.Run("AR の位置が SfM と食い違う", func(t *testing.T) {
		poses, ar := orbitPair(20, 1, 0.5, id, [3]float64{}, "a")
		for i := range ar {
			ar[i].Position[0] += 0.3 * math.Sin(float64(i*7))
		}
		if _, ok := AlignToAR(poses, ar); ok {
			t.Error("ずれが大きいのに合わせた")
		}
	})
}

func TestAlignToARPicksLargestSession(t *testing.T) {
	id := [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	poses, ar := orbitPair(20, 1, 0.5, id, [3]float64{}, "big")
	// 別の回（座標系が違う）で撮った 6 枚を足す。縮尺も違う値にしておく。
	for i := 0; i < 6; i++ {
		ar[i] = ARPose{Name: ar[i].Name, Session: "small", Position: [3]float64{ar[i].Position[0] * 2, ar[i].Position[1] * 2, ar[i].Position[2] * 2}}
	}
	al, ok := AlignToAR(poses, ar)
	if !ok || al.Session != "big" || math.Abs(al.Scale-0.5) > 1e-6 {
		t.Errorf("session = %q, scale = %v, ok = %v", al.Session, al.Scale, ok)
	}
}

func TestARPosesOf(t *testing.T) {
	frames := []scan.Frame{
		{FileName: "frame_0001.jpg", Camera: []byte(`{"capturedAt":"x","arPose":{"session":"s1","position":[1,2,3],"orientation":[0,0,0,1]}}`)},
		{FileName: "frame_0002.jpg", Camera: []byte(`{"focalLength35mm":26}`)},
		{FileName: "frame_0003.jpg", Camera: []byte(`{"arPose":{"session":"","position":[1,2,3]}}`)},
		{FileName: "frame_0004.jpg", Camera: []byte(`{"arPose":{"session":"s1","position":[1e9,0,0]}}`)},
		{FileName: "frame_0005.jpg"},
	}
	got := ARPosesOf(frames)
	if len(got) != 1 || got[0].Name != "frame_0001.jpg" || got[0].Session != "s1" || got[0].Position != [3]float64{1, 2, 3} {
		t.Errorf("got %+v", got)
	}
}
