package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/config"
)

func mapEnv(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

func TestRequired_無ければまとめて落ちる(t *testing.T) {
	l := config.NewWith(mapEnv(map[string]string{"MYSQL_DSN": "dsn://x"}))
	dsn := l.Required("MYSQL_DSN")
	_ = l.Required("SECRET_KEY") // 無い
	_ = l.Required("API_URL")    // 無い

	if dsn != "dsn://x" {
		t.Errorf("DSN=%q", dsn)
	}
	err := l.Err()
	if !errors.Is(err, config.ErrMissingConfig) {
		t.Fatalf("必須欠けを検出できていない: %v", err)
	}
	// 1つずつではなく、まとめて報告する
	if !strings.Contains(err.Error(), "SECRET_KEY") || !strings.Contains(err.Error(), "API_URL") {
		t.Errorf("欠けたキーがまとめて出ていない: %v", err)
	}
}

func TestOptional_無くても既定で動く(t *testing.T) {
	l := config.NewWith(mapEnv(map[string]string{}))
	if got := l.Optional("PORT", "8080"); got != "8080" {
		t.Errorf("Optional=%q", got)
	}
	if got := l.Duration("TIMEOUT", 3*time.Second); got != 3*time.Second {
		t.Errorf("Duration=%v", got)
	}
	if got := l.Int("PAGE", 100); got != 100 {
		t.Errorf("Int=%d", got)
	}
	if got := l.Bool("DEBUG", false); got != false {
		t.Errorf("Bool=%v", got)
	}
	// Optional だけなら起動できる（fail-fast しない）
	if err := l.Err(); err != nil {
		t.Errorf("Optional だけで落ちてはいけない: %v", err)
	}
}

func TestOptional_後から足しても既存に影響しない(t *testing.T) {
	// 既存の env（新キーを知らない）でも、Optional は既定で動く。
	old := config.NewWith(mapEnv(map[string]string{"PORT": "9090"}))
	port := old.Optional("PORT", "8080")
	// 後から足した新キーは、旧環境では既定になる
	newFeature := old.Optional("NEW_FEATURE_FLAG", "off")
	if port != "9090" || newFeature != "off" {
		t.Errorf("port=%q new=%q", port, newFeature)
	}
}

func TestDuration_壊れた値はErrに積む(t *testing.T) {
	l := config.NewWith(mapEnv(map[string]string{"TIMEOUT": "5 minutes"})) // 不正
	d := l.Duration("TIMEOUT", time.Second)
	if d != time.Second {
		t.Errorf("壊れた値は既定に倒すべき: %v", d)
	}
	if l.Err() == nil {
		t.Error("壊れた期間は Err に積むべき")
	}
}

func TestAudit_値は出さずキーと出所だけ(t *testing.T) {
	l := config.NewWith(mapEnv(map[string]string{"MYSQL_DSN": "secret-dsn"}))
	_ = l.Required("MYSQL_DSN")
	_ = l.Optional("PORT", "8080")
	audit := strings.Join(l.Audit(), " ")
	if !strings.Contains(audit, "MYSQL_DSN=env") || !strings.Contains(audit, "PORT=default:8080") {
		t.Errorf("audit=%q", audit)
	}
	// ★値そのものが漏れていないこと
	if strings.Contains(audit, "secret-dsn") {
		t.Error("Audit に値が漏れている（キーと出所だけにすべき）")
	}
}
