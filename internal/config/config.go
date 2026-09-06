// Package config は環境変数を1箇所で読む。
//
// 散らばった os.Getenv は事故のもと（どこで何を読むかが追えない）。
// ここに集約し、Load() で一度だけ読む。
//
// ★2種類を型で分ける:
//   - Required: 無ければ**起動時に落とす**（DSN・鍵など。既定で黙って動かさない = fail-fast）。
//   - Optional: 無くても既定で動く（タイムアウト・ページサイズなど。後から足しても既存に影響しない）。
//
// 「なくても動く」を全部に適用すると、本番で鍵未設定のまま起動する事故になる。
// だから危険なものは Required、無害なものだけ Optional。
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Getenv は環境変数の読み取り（テストで差し替えられるように）。
type Getenv func(string) string

// Loader は1回の読み取りセッション。欠けた Required を貯めてまとめて報告する
// （1つずつ落とすと、直しては再起動を繰り返すことになる）。
type Loader struct {
	get     Getenv
	missing []string
	used    map[string]string // 監査: 実際に読んだキーと出所（env/default）
}

// New は os.Getenv を使う Loader。
func New() *Loader { return NewWith(os.Getenv) }

// NewWith は任意の Getenv を使う（テスト用）。
func NewWith(g Getenv) *Loader {
	return &Loader{get: g, used: map[string]string{}}
}

// Required は必須の文字列。無ければ Err() に積む。
func (l *Loader) Required(key string) string {
	v := strings.TrimSpace(l.get(key))
	if v == "" {
		l.missing = append(l.missing, key)
		l.used[key] = "MISSING(required)"
		return ""
	}
	l.used[key] = "env"
	return v
}

// Optional は既定つきの文字列（無くても動く）。
func (l *Loader) Optional(key, def string) string {
	v := strings.TrimSpace(l.get(key))
	if v == "" {
		l.used[key] = "default:" + def
		return def
	}
	l.used[key] = "env"
	return v
}

// Duration は既定つきの time.Duration。壊れた値は Err() に積む。
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(l.get(key))
	if raw == "" {
		l.used[key] = "default:" + def.String()
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.missing = append(l.missing, fmt.Sprintf("%s（%q は期間として読めない: %v）", key, raw, err))
		return def
	}
	l.used[key] = "env"
	return d
}

// Int は既定つきの整数。壊れた値は Err() に積む。
func (l *Loader) Int(key string, def int) int {
	raw := strings.TrimSpace(l.get(key))
	if raw == "" {
		l.used[key] = fmt.Sprintf("default:%d", def)
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.missing = append(l.missing, fmt.Sprintf("%s（%q は整数として読めない）", key, raw))
		return def
	}
	l.used[key] = "env"
	return n
}

// Bool は既定つきの真偽。1/true/yes/on を真とみなす。
func (l *Loader) Bool(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(l.get(key)))
	if raw == "" {
		l.used[key] = fmt.Sprintf("default:%v", def)
		return def
	}
	l.used[key] = "env"
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		l.missing = append(l.missing, fmt.Sprintf("%s（%q は真偽として読めない）", key, raw))
		return def
	}
}

// ErrMissingConfig は必須設定が欠けているときのエラー。
var ErrMissingConfig = errors.New("必須の設定が無い")

// Err は欠けた Required（と壊れた値）をまとめて返す。1つも無ければ nil。
func (l *Loader) Err() error {
	if len(l.missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrMissingConfig, strings.Join(l.missing, ", "))
}

// Audit は「どのキーを env から読んだか / 既定を使ったか」の一覧（起動ログ用）。
// ★値そのものは出さない（DSN や鍵が漏れる）。キーと出所だけ。
func (l *Loader) Audit() []string {
	keys := make([]string, 0, len(l.used))
	for k := range l.used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+l.used[k])
	}
	return out
}
