// Package model は、ワーカーが扱う「対象の状態」を表す型を定義する。
//
// 資料 §6.3 / §4.2 の指針をコードで表現している。
//   - 列挙は string ではなく小さな整数型（打ち間違いをコンパイルエラーにする）
//   - 比較対象の struct に観測時刻を入れない（入れると 100% 変化ありになる）
//   - 浮動小数は丸めてから比較する
package model

import (
	"fmt"
	"time"
)

// Status は対象の稼働状態。
//
// なぜ string ではなく uint8 か（§6.3）:
// 速度目的ではない。string にすると "runing" のような打ち間違いがコンパイルを
// 通ってしまい、DB の値と永遠に一致せず「変化あり」と判定され続ける。
// 整数型なら未定義の識別子はコンパイルエラーになる。
type Status uint8

const (
	// StatusUnknown は「こちらが観測できていない」ことを表す（§2.4）。
	// オフラインとは別の値であることが重要。
	StatusUnknown Status = iota
	StatusRunning
	StatusStopped
	StatusError
)

var statusNames = [...]string{"unknown", "running", "stopped", "error"}

func (s Status) String() string {
	if int(s) >= len(statusNames) {
		return fmt.Sprintf("Status(%d)", uint8(s))
	}
	return statusNames[s]
}

// ParseStatus は API / WebSocket から来た文字列を Status に変換する。
// 知らない値は StatusUnknown と false を返す。ここで落とさないのが §6.3 の方針
// （API に値が1つ増えただけで全件同期が止まってはいけない）。
func ParseStatus(s string) (Status, bool) {
	for i, name := range statusNames {
		if name == s {
			return Status(i), true
		}
	}
	return StatusUnknown, false
}

// Source は、その観測がどこから来たか（§2.4 の source 列）。
type Source uint8

const (
	SourceUnknown Source = iota
	SourceAPI            // 正しさを担保する側（§2.1）
	SourceWS             // 速く気づくための最適化（§2.1）
)

var sourceNames = [...]string{"unknown", "api", "ws"}

func (s Source) String() string {
	if int(s) >= len(sourceNames) {
		return fmt.Sprintf("Source(%d)", uint8(s))
	}
	return sourceNames[s]
}

// State は「比較する中身だけ」を集めた struct（§4.2）。
//
// ★ここに観測時刻を入れてはいけない。毎回値が違うので 100% 変化ありと判定され、
// 「変化がなければ書かない」という仕組みが丸ごと無効になる。しかも動いてはいる
// ので気づかない。TestStateHasNoTimestamp がこれを機械的に検査している。
//
// 比較可能（comparable）であること自体が設計の一部で、`==` 一発で差分判定できる。
type State struct {
	Status  Status
	Online  bool
	Battery int8 // 丸めた値（§4.2 の「浮動小数をそのまま比較しない」）
}

// RoundBattery は電池残量(%)を比較用に丸める（§4.2）。
// 「どれだけ変わったら変化と呼ぶか」は業務判断なので、刻み幅を引数で受ける。
func RoundBattery(pct float64, step int) int8 {
	if step <= 0 {
		step = 1
	}
	switch {
	case pct < 0:
		pct = 0
	case pct > 100:
		pct = 100
	}
	v := (int(pct+float64(step)/2) / step) * step
	if v > 100 {
		v = 100
	}
	return int8(v)
}

// Observation は「いつ・どこから観測したか」を含む1件の観測結果。
// State と分けてあるのが要点で、比較には State だけを使う。
type Observation struct {
	State      State
	ObservedAt time.Time
	Source     Source
}

// ID は対象（ロボット）の識別子。
type ID string

// TenantID はテナント識別子。
type TenantID string
