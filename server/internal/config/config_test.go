package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// env は map を getenv 関数にする（テストでは本物の環境変数を使わない）。
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// minimal は起動に最低限必要な設定。
func minimal(extra map[string]string) map[string]string {
	m := map[string]string{
		"DATABASE_URL": "postgres://scanner:pw@localhost:5432/scanner?sslmode=disable",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(minimal(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.DataDir != "/data" {
		t.Errorf("既定値 = %+v", cfg)
	}
	// Cloudflare（Free/Pro）の上限 100MB より小さいこと。
	if cfg.Limits.MaxUploadBytes >= 100*1024*1024 {
		t.Errorf("MaxUploadBytes = %d（100MB 未満のはず）", cfg.Limits.MaxUploadBytes)
	}
	if cfg.Jobs.Retention != 24*time.Hour || cfg.Jobs.MaxAttempts != 2 || !cfg.Jobs.KeepCompleted ||
		!cfg.Jobs.DeferSplat || !cfg.Jobs.SplatYield {
		t.Errorf("Jobs = %+v", cfg.Jobs)
	}
	p := cfg.Pipeline
	// GPU の無い環境で安全に動く既定値になっていること。
	if p.ColmapUseGPU || !p.SplatForceCPU || p.MeshMode != MeshModeAuto || !p.RequireTools {
		t.Errorf("Pipeline = %+v", p)
	}
}

// TestDatabaseURLFromParts: パスワードに URL の区切り文字が入っていても、正しい接続文字列になる。
func TestDatabaseURLFromParts(t *testing.T) {
	password := "p@ss:w/rd?#&=%"
	cfg, err := Load(env(map[string]string{
		"DB_HOST":     "scanner-postgresql",
		"DB_USER":     "scanner",
		"DB_PASSWORD": password,
		"DB_NAME":     "scanner",
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("組み立てた URL が解釈できない: %v", err)
	}
	got, _ := u.User.Password()
	if got != password {
		t.Errorf("パスワード = %q, want %q", got, password)
	}
	if u.Host != "scanner-postgresql:5432" || u.Path != "/scanner" || u.User.Username() != "scanner" || u.Query().Get("sslmode") != "disable" {
		t.Errorf("URL = %s", cfg.DatabaseURL)
	}
	// DATABASE_URL があればそちらを優先する。
	cfg, _ = Load(env(minimal(map[string]string{"DB_HOST": "ignored"})))
	if !strings.Contains(cfg.DatabaseURL, "localhost:5432") {
		t.Errorf("DATABASE_URL が優先されていない: %s", cfg.DatabaseURL)
	}
}

// TestLoadReportsAllProblemsAtOnce: 問題は1つずつではなく、まとめて報告する。
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"MIN_IMAGES":       "十枚",   // 数値でない
		"MAX_IMAGES":       "2",    // MIN_IMAGES（既定 8）より小さい
		"RETENTION":        "1day", // Go の時間表記でない
		"MESH_MODE":        "fancy",
		"SPLAT_ITERATIONS": "0",
	}))
	if err == nil {
		t.Fatal("不正な設定でエラーにならない")
	}
	for _, want := range []string{"DATABASE_URL", "MIN_IMAGES", "MAX_IMAGES", "RETENTION", "MESH_MODE", "SPLAT_ITERATIONS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("エラーに %s の指摘が無い:\n%v", want, err)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(minimal(map[string]string{
		"MAX_UPLOAD_MB": "50", "RETENTION": "48h", "MESH_MODE": "sparse", "NUM_THREADS": "4",
		"SPLAT_ITERATIONS": "500", "SPLAT_DOWNSCALE": "4", "ENABLE_SPLAT": "false",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxUploadBytes != 50*1024*1024 || cfg.Jobs.Retention != 48*time.Hour {
		t.Errorf("上書きが効いていない: %+v / %+v", cfg.Limits, cfg.Jobs)
	}
	p := cfg.Pipeline
	if p.MeshMode != MeshModeSparse || p.NumThreads != 4 || p.SplatIterations != 500 || p.SplatDownscale != 4 || p.EnableSplat {
		t.Errorf("Pipeline = %+v", p)
	}
}

// TestLoadPipelineStandalone: 単体実行ツール用の読み込みは、DB や認証の設定を要求しない。
func TestLoadPipelineStandalone(t *testing.T) {
	p, err := LoadPipeline(env(map[string]string{"SPLAT_ITERATIONS": "300"}))
	if err != nil || p.SplatIterations != 300 || p.ColmapBin != "colmap" {
		t.Fatalf("p = %+v, err = %v", p, err)
	}
	if _, err := LoadPipeline(env(map[string]string{"MESH_MODE": "x"})); err == nil {
		t.Fatal("不正な MESH_MODE でエラーにならない")
	}
}
