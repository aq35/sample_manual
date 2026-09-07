// Package readrouter は「読みを primary とレプリカで振り分ける」ルータ。
//
// EXP-20 の結論を形にしたもの:
//   - 書いた直後・自分の書き込み（read-your-writes）→ **primary**
//   - worker の dispatch ポーリング（pending/due の取得）→ **primary**（レプリカだと二重発行/取りこぼし）
//   - 不変な過去（過去の実績・履歴・レポート・admin の横断）→ **レプリカ**（ラグ許容）
//
// レプリカはレプリケーションラグがあるので、「今書いたものが即見える」保証は無い。
// だから何でもレプリカへ回してはいけない。ここで意図を型にして振り分ける。
package readrouter

import "database/sql"

// Freshness は読みが要求する新しさ。
type Freshness int

const (
	// Strong: 自分の書き込みや直近の変更が必ず見えないと困る（read-your-writes）。→ primary
	Strong Freshness = iota
	// Eventual: 少し古くてもよい。不変な過去・レポート。→ レプリカ（あれば）
	Eventual
)

// Router は primary とレプリカを持ち、Freshness で使う DB を選ぶ。
type Router struct {
	primary *sql.DB
	replica *sql.DB // nil ならすべて primary
}

// New はルータを作る。replica が nil ならレプリカ無し構成（すべて primary）。
func New(primary, replica *sql.DB) *Router {
	return &Router{primary: primary, replica: replica}
}

// DB は Freshness に応じた *sql.DB を返す。
//
// ★worker の dispatch ポーリングや read-your-writes は Strong を渡すこと。
// レプリカが無い（nil）ときは常に primary。
func (r *Router) DB(f Freshness) *sql.DB {
	if f == Eventual && r.replica != nil {
		return r.replica
	}
	return r.primary
}

// Primary は必ず primary（書き込み・dispatch・read-your-writes 用）。
func (r *Router) Primary() *sql.DB { return r.primary }

// HasReplica はレプリカが構成されているか。
func (r *Router) HasReplica() bool { return r.replica != nil }
