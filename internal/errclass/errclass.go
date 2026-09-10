// Package errclass は EXP-68（エラー分類の型付けと取りこぼし検出）の実験本体。
//
// EXP-49 の分類は bool（Retryable か否か）だった。だが「retry してよいか」だけだと、
// **恒久エラー**と**分からないエラー**が同じ false に潰れる。EXP-1 の OUTCOME_UNKNOWN と同じで、
// 「知らない」を「恒久」と言い切るのは嘘。3値（Transient / Permanent / Unknown）で持つと、
//   - retry 判断（Transient だけ retry）
//   - 下流の扱い（Permanent は DLQ+アラート=EXP-45 / Unknown は『不明』として surface）
//
// を分けられる。さらに Kind を足したときの「switch の case 取りこぼし」は EXP-67 の
// exhaustive 検査が拾う（Go に sum type が無い弱点を、型＋静的解析で補う）。
package errclass

import (
	"database/sql/driver"
	"errors"

	"github.com/go-sql-driver/mysql"
)

// Kind はエラーの分類。3値で持つ（bool に潰さない）。
type Kind int

const (
	// Unknown は分類できないエラー。retry もせず（fail-safe）、恒久とも言い切らない。
	// EXP-1 の OUTCOME_UNKNOWN と同じ思想＝「知らない」を「知っている」と偽らない。
	Unknown Kind = iota
	// Transient は一時エラー（deadlock 1213 / lock wait timeout 1205 / 接続断）。retry してよい。
	Transient
	// Permanent は恒久エラー（重複キー 1062・制約違反・構文）。retry せず fail-fast、DLQ+アラート。
	Permanent
)

func (k Kind) String() string {
	switch k {
	case Transient:
		return "transient"
	case Permanent:
		return "permanent"
	case Unknown:
		return "unknown"
	}
	return "unknown"
}

// Classify は err を3値に分類する。認識できないものは Unknown（Permanent と混ぜない）。
func Classify(err error) Kind {
	if err == nil {
		return Unknown // 呼ぶ側が nil を渡すのは誤用。ここでは Unknown 扱い
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1213, 1205: // Deadlock / Lock wait timeout
			return Transient
		case 1062, 1452, 1264, 3819, 1064:
			// 重複キー / FK 違反 / 範囲外 / CHECK 違反 / 構文（EXP-65 の入口ガードが弾くはずのもの）
			return Permanent
		default:
			return Unknown // 知らない番号を「恒久」と決めつけない
		}
	}
	if errors.Is(err, driver.ErrBadConn) {
		return Transient // 接続断は張り直して retry（EXP-33）
	}
	return Unknown
}

// ShouldRetry は retry してよいか。Transient だけ true。
//
// ★この switch は Kind を網羅している。Kind を足したら EXP-67 の exhaustive 検査が
// ここを「未網羅」と指摘する＝取りこぼしがコンパイル/CI 前に分かる。
func ShouldRetry(k Kind) bool {
	switch k {
	case Transient:
		return true
	case Permanent:
		return false
	case Unknown:
		return false // 分からないものは retry しない（fail-safe）。ただし下流で『不明』として surface する
	}
	return false
}

// NaiveRetryUnlessKnownPermanent は「恒久だと分かるもの以外は retry する」というよくある素朴な判断。
// これだと **Unknown を Transient 扱いして無限 retry** する（恒久だが未知の番号で延々叩く）。
// 比較用（＝事故側）。
func NaiveRetryUnlessKnownPermanent(err error) bool {
	return Classify(err) != Permanent // Unknown も retry してしまう
}
