package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLockNotAcquired は、待っても排他ロックが取れなかったとき。
var ErrLockNotAcquired = errors.New("排他ロックを取れなかった")

// WithLock は MySQL のユーザーロック（GET_LOCK）で fn を排他実行する。
//
// **必ず1本の接続を固定して使う。** 実験（TestExperimentLock_接続に紐づく）のとおり、
// *sql.DB のメソッドで GET_LOCK を呼ぶと、取得と解放が別々の接続に行き、
//   - RELEASE_LOCK が 0 を返して解放できない
//   - ロックを持ったままの接続がプールに戻り、次にそれを借りた処理が他人のロックを解放できる
//
// という壊れ方をする。ここではその轍を踏まないよう、接続の固定を内部に閉じ込めてある。
//
// 向いている用途:
//   - **マイグレーション**（DDL の暗黙コミットで外れないのは、この方式だけ）
//   - 数秒〜数十秒で終わる、プロセス間で1つだけ走らせたい処理（日次の集計など）
//
// 向いていない用途:
//   - **長時間の「担当」**（ワーカーの担当テナントなど）。接続を占有し続けるうえ、
//     期限が無いのでアプリが固まると永久に持ち続ける。→ internal/lease の期限つきリースを使う
//   - **レプリカをまたぐ排他**。ユーザーロックはレプリケーションされず、
//     フェイルオーバーで消える
func (d *DB) WithLock(ctx context.Context, name string, wait time.Duration, fn func(context.Context) error) error {
	if wait < 0 {
		wait = 0
	}
	// ★接続を固定する。ここが本質
	conn, err := d.sqldb.Conn(ctx)
	if err != nil {
		return fmt.Errorf("排他ロック用の接続を取れない: %w", err)
	}
	defer func() { _ = conn.Close() }()

	full, err := d.lockName(ctx, conn, name)
	if err != nil {
		return err
	}

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", full, int(wait.Seconds())).Scan(&got); err != nil {
		return fmt.Errorf("GET_LOCK(%s): %w", full, err)
	}
	if !got.Valid {
		return fmt.Errorf("GET_LOCK(%s): NULL が返った（名前が長すぎるなど）", full)
	}
	if got.Int64 != 1 {
		return fmt.Errorf("%w: %s（%v 待った）", ErrLockNotAcquired, full, wait)
	}
	d.cnt.locks.Add(1)

	start := time.Now()
	defer func() {
		// 解放は必ず同じ接続で行う（この関数の中では conn しか使っていない）
		var released sql.NullInt64
		relCtx := context.WithoutCancel(ctx)
		if err := conn.QueryRowContext(relCtx, "SELECT RELEASE_LOCK(?)", full).Scan(&released); err != nil {
			d.opt.Logger.Error("RELEASE_LOCK に失敗（接続を閉じれば解放される）", "lock", full, "err", err)
			return
		}
		if !released.Valid || released.Int64 != 1 {
			d.opt.Logger.Error("RELEASE_LOCK が 1 を返さなかった", "lock", full, "result", released)
		}
		// 長く持つ用途なら設計が違う（接続を占有し続ける。docs/locking.md 3.7）
		if held := time.Since(start); held > d.opt.MaxLockHold {
			d.opt.Logger.Warn("排他ロックを長く持ちすぎている（接続を1本占有している。期限つきリースを検討）",
				"lock", full, "保持時間", held)
		}
	}()

	return fn(ctx)
}

// lockName はロック名にスキーマ名を付ける。
//
// 実験（TestExperimentLock_名前はサーバ全体で共通）のとおり、ユーザーロックの名前は
// **サーバ全体で共通**。同じサーバに同居している別アプリ・別スキーマと衝突する。
func (d *DB) lockName(ctx context.Context, conn *sql.Conn, name string) (string, error) {
	schema, _ := d.schema.Load().(string)
	if schema == "" {
		var got sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&got); err != nil {
			return "", fmt.Errorf("スキーマ名を取れない: %w", err)
		}
		schema = got.String
		d.schema.Store(schema)
	}
	full := schema + "." + name
	if len(full) > 64 {
		// MySQL 8.0 のロック名は 64 文字まで（超えるとエラーになる）
		return "", fmt.Errorf("ロック名が長すぎる（64文字まで）: %s", full)
	}
	return full, nil
}

// LockHolder は、そのロックを現在持っている接続 ID を返す（誰も持っていなければ false）。
// 運用中に「何が詰まっているか」を見るために使う。
func (d *DB) LockHolder(ctx context.Context, name string) (int64, bool, error) {
	conn, err := d.sqldb.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	full, err := d.lockName(ctx, conn, name)
	if err != nil {
		return 0, false, err
	}
	var id sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?)", full).Scan(&id); err != nil {
		return 0, false, err
	}
	return id.Int64, id.Valid, nil
}
