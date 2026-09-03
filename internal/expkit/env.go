// Package expkit は実験のための測定器。
//
// この基盤が満たすべきこと（実験指示書の成果条件）:
//  1. 事故を再現できる（故障注入）
//  2. 防止策で事故が止まる（同じ条件で before / after）
//  3. 防止策を外すとテストが落ちる（counter-proof を明示的に持つ）
//  4. 測定条件が保存される（Env を結果ファイルに必ず埋める）
//  5. 適用範囲と未保証範囲が書かれる（Scope / Uncertainty）
//  6. 他プロジェクトへ持ち込める（このパッケージは repo 固有のものに依存しない）
//
// 数字だけを見て結論を出さないために、結果ファイルには
// 「いつ・どの SHA で・どんな環境で・どんな負荷と故障注入で」を必ず一緒に残す。
package expkit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Env は測定条件。結果ファイルの先頭に必ず入る。
type Env struct {
	CapturedAt   time.Time         `json:"captured_at"`
	GoVersion    string            `json:"go_version"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	NumCPU       int               `json:"num_cpu"`
	GOMAXPROCS   int               `json:"gomaxprocs"`
	Hostname     string            `json:"hostname"`
	GitSHA       string            `json:"git_sha"`
	GitDirty     bool              `json:"git_dirty"`
	MySQLVersion string            `json:"mysql_version,omitempty"`
	MySQLVars    map[string]string `json:"mysql_vars,omitempty"`
	Notes        []string          `json:"notes,omitempty"`
}

// 記録しておく MySQL のサーバ変数。結果の再現に効くものだけ。
var mysqlVarsOfInterest = []string{
	"innodb_flush_log_at_trx_commit",
	"innodb_buffer_pool_size",
	"innodb_lock_wait_timeout",
	"max_connections",
	"wait_timeout",
	"transaction_isolation",
	"sync_binlog",
	"log_bin",
}

// CaptureEnv は測定条件を集める。db が nil なら MySQL の情報は空になる。
func CaptureEnv(ctx context.Context, db *sql.DB) Env {
	host, _ := os.Hostname()
	e := Env{
		CapturedAt: time.Now().UTC(),
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Hostname:   host,
	}
	e.GitSHA, e.GitDirty = gitState()

	if db == nil {
		return e
	}
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&e.MySQLVersion); err != nil {
		e.Notes = append(e.Notes, fmt.Sprintf("MySQL のバージョンを取れなかった: %v", err))
		return e
	}
	e.MySQLVars = map[string]string{}
	for _, name := range mysqlVarsOfInterest {
		var k, v string
		//smlint:allow loopquery 理由: 測定条件の採取。起動時に一度だけ、固定の 8 変数
		if err := db.QueryRowContext(ctx, "SHOW VARIABLES LIKE ?", name).Scan(&k, &v); err == nil {
			e.MySQLVars[k] = v
		}
	}
	return e
}

// gitState は実行時のコミットと、作業ツリーが汚れているかを返す。
// 「どの SHA の結果か」が分からない測定は、後から検証できない。
func gitState() (string, bool) {
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	dirty := false
	if out, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		dirty = strings.TrimSpace(string(out)) != ""
	}
	return strings.TrimSpace(string(sha)), dirty
}

func (e Env) String() string {
	dirty := ""
	if e.GitDirty {
		dirty = "+dirty"
	}
	return fmt.Sprintf("%s %s/%s cpu=%d gomaxprocs=%d mysql=%s sha=%.12s%s",
		e.GoVersion, e.OS, e.Arch, e.NumCPU, e.GOMAXPROCS, e.MySQLVersion, e.GitSHA, dirty)
}
