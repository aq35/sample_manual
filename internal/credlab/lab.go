// Package credlab は EXP-13（DB 資格情報のローテーション）の実験本体。
//
// 問い: DB のユーザー/パスワードが期限切れ（ローテーション）するとき、どうあるべきか。
//
// 事実（EXP-5 と地続き）:
//   - プール内の**既に張られた接続は、パスワードを変えても切れない**（サーバが切るまで有効）。
//   - 期限が効くのは**新しい接続を張るとき**。だから ConnMaxLifetime を資格情報の期限より短くして、
//     期限が来る前に接続を自然に張り替える。
//   - 資格情報を更新したら、**新しい DSN で新プールを作り、原子的に差し替え、古いプールをドレイン**する。
//     全接続を一斉に切ると EXP-5 の飽和事故になる。graceful に入れ替える。
//
// この実験は「abrupt（古いプールのまま）」と「graceful（新プールへ差し替え）」を比べ、
// 切り替え中の接続エラー数を測る。
package credlab

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
)

const user = "exp13"

// mysqlRoot は root 権限の操作（ユーザー管理）を socket 経由で行う。
func mysqlRoot(sqlText string) error {
	cmd := exec.Command("mysql", "-uroot", "-e", sqlText)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mysql root: %w\n%s", err, out)
	}
	return nil
}

// SetupUser は exp13 ユーザーを pw で作り直し、workerdb への SELECT を許す。
func SetupUser(pw string) error {
	return mysqlRoot(fmt.Sprintf(
		"DROP USER IF EXISTS '%s'@'%%'; CREATE USER '%s'@'%%' IDENTIFIED BY '%s'; "+
			"GRANT SELECT ON workerdb.* TO '%s'@'%%'; FLUSH PRIVILEGES;", user, user, pw, user))
}

// Rotate は exp13 のパスワードを新しい値へ即座に変える（古いパスワードは無効になる）。
// abrupt な切り替えの模擬。
func Rotate(newPw string) error {
	return mysqlRoot(fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED BY '%s'; FLUSH PRIVILEGES;", user, newPw))
}

// RotateDual は MySQL 8.0 の二重パスワードでローテーションする。
//
// ★これがゼロダウンタイム移行の鍵。RETAIN CURRENT PASSWORD で
// **古いパスワードも新しいパスワードも両方有効**な期間を作る。
// その間に新プールへ差し替えれば、古いプールの接続（古いパスワード）も弾かれない。
func RotateDual(newPw string) error {
	return mysqlRoot(fmt.Sprintf(
		"ALTER USER '%s'@'%%' IDENTIFIED BY '%s' RETAIN CURRENT PASSWORD; FLUSH PRIVILEGES;", user, newPw))
}

// DiscardOldPassword は移行が終わったあと、古いパスワードを無効にする。
func DiscardOldPassword() error {
	return mysqlRoot(fmt.Sprintf("ALTER USER '%s'@'%%' DISCARD OLD PASSWORD; FLUSH PRIVILEGES;", user))
}

// DropUser は後片付け。
func DropUser() error {
	return mysqlRoot(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%';", user))
}

// DSN は baseDSN（worker の DSN）から host/port を借りて、exp13 の DSN を作る。
func DSN(baseDSN, pw string) (string, error) {
	c, err := mysql.ParseDSN(baseDSN)
	if err != nil {
		return "", err
	}
	c.User = user
	c.Passwd = pw
	c.DBName = "workerdb"
	return c.FormatDSN(), nil
}

// OpenPool は exp13 用のプールを開く。connLifetime を短くして、期限前に接続を張り替える。
func OpenPool(dsn string, connLifetime time.Duration) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(connLifetime) // ★資格情報の期限より短く
	return db, nil
}

// Holder は「今使うプール」を原子的に持つ。負荷 goroutine はここから読む。
type Holder struct{ p atomic.Pointer[sql.DB] }

func NewHolder(db *sql.DB) *Holder {
	h := &Holder{}
	h.p.Store(db)
	return h
}
func (h *Holder) Get() *sql.DB { return h.p.Load() }

// Swap は新プールへ差し替え、古いプールを返す（呼び出し側がドレインして閉じる）。
func (h *Holder) Swap(next *sql.DB) *sql.DB { return h.p.Swap(next) }

// LoadResult は負荷の結果。
type LoadResult struct {
	Ops      int64
	AuthErr  int64 // 認証エラー（Error 1045）
	OtherErr int64
	Elapsed  time.Duration
}

// RunLoad は holder の現在プールに対して、concurrency 本で dur だけ SELECT を打ち続ける。
// 認証エラー（新接続がローテ後の古いパスワードで弾かれる）を数える。
func RunLoad(ctx context.Context, h *Holder, concurrency int, dur time.Duration) LoadResult {
	var ops, authErr, otherErr atomic.Int64
	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				db := h.Get()
				var one int
				//smlint:allow loopquery 理由: 負荷生成。ローテ中の接続エラーを数えるのが目的
				err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
				switch {
				case err == nil:
					ops.Add(1)
				case isAuthErr(err):
					authErr.Add(1)
				default:
					otherErr.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	return LoadResult{Ops: ops.Load(), AuthErr: authErr.Load(),
		OtherErr: otherErr.Load(), Elapsed: time.Since(start)}
}

func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "1045") || strings.Contains(s, "access denied")
}

// GracefulSwap は「新パスワードで新プールを開き、原子的に差し替え、古いプールをドレインして閉じる」。
// これが資格情報ローテーションの正しい入れ替え方。
func GracefulSwap(h *Holder, baseDSN, newPw string, connLifetime, drain time.Duration) error {
	dsn, err := DSN(baseDSN, newPw)
	if err != nil {
		return err
	}
	next, err := OpenPool(dsn, connLifetime)
	if err != nil {
		return err
	}
	// 先に1本 ping して、新プールが本当に使えることを確かめてから差し替える。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := next.PingContext(ctx); err != nil {
		_ = next.Close()
		return fmt.Errorf("新プールが使えない: %w", err)
	}
	old := h.Swap(next)
	// 古いプールは即閉じない。使用中のクエリが終わるまでドレインしてから閉じる。
	go func() {
		time.Sleep(drain)
		_ = old.Close()
	}()
	return nil
}
