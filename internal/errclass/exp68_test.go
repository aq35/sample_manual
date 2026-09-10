package errclass_test

// EXP-68: エラー分類を bool でなく3値(Transient/Permanent/Unknown)の型で持ち、取りこぼしを検出する。
//
//	go test ./internal/errclass/ -run TestEXP68 -v
//
// bool（retry してよいか）だと Permanent と Unknown が同じ false に潰れ、
//   ① 「知らない」を「恒久」と偽る（EXP-1 OUTCOME_UNKNOWN 違反・下流の扱いを分けられない）
//   ② 素朴な『恒久以外は retry』設計だと Unknown を無限 retry する
// が起きる。3値の型で持てば両方を避けられ、Kind を足したときの switch 取りこぼしは EXP-67 が拾う。

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/aq35/sample_manual/internal/errclass"
	"github.com/aq35/sample_manual/internal/expkit"
	"github.com/aq35/sample_manual/internal/retrylab"
)

func TestEXP68_エラー分類は3値の型で持つ(t *testing.T) {
	ctx := context.Background()
	rec := expkit.NewRecorder("EXP-68", "error-classification",
		"エラー分類を bool でなく3値(Transient/Permanent/Unknown)で持ち、取りこぼしを検出する")
	rec.Env(expkit.CaptureEnv(ctx, nil))
	rec.Freeze(strings.Join([]string{
		"bool(retry可否)だと Permanent と Unknown が同じ false に潰れ、『知らない』を『恒久』と偽る",
		"（下流の扱い＝DLQ+アラート vs 不明として surface を分けられない）。",
		"3値の型で持てば両者を区別でき、かつ『恒久以外は retry』の素朴設計が Unknown を無限 retry する事故も避けられる。",
		"Kind を足したときの switch 取りこぼしは EXP-67 の exhaustive 検査が拾う。",
	}, " "))

	// 代表的なエラー3種
	transient := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}      // 一時
	permanent := &mysql.MySQLError{Number: 1062, Message: "Duplicate key"} // 恒久
	unknown := errors.New("外部APIが未知の理由で失敗")                                // 分からない
	_ = driver.ErrBadConn

	// ---- ① bool は Permanent と Unknown を区別できない ----
	// retrylab.Retryable（EXP-49・bool）: transient=true, それ以外=false
	boolBuckets := map[bool][]string{}
	for _, c := range []struct {
		name string
		err  error
	}{{"transient", transient}, {"permanent", permanent}, {"unknown", unknown}} {
		b := retrylab.Retryable(c.err)
		boolBuckets[b] = append(boolBuckets[b], c.name)
	}
	// false のバケツに permanent と unknown が同居している＝区別できない
	falseBucket := boolBuckets[false]
	rec.Add(expkit.Variant{
		Name:     "bool 分類（EXP-49）: Permanent と Unknown が潰れる",
		Accident: true,
		Metrics:  map[string]float64{"false_bucket_size": float64(len(falseBucket))},
		Notes:    []string{"retry不可バケツ = [" + strings.Join(falseBucket, ", ") + "]。恒久と不明が同居し、下流で分けられない"},
	})

	// ---- ② 3値なら3つに分かれる ----
	kinds := map[string]errclass.Kind{
		"transient": errclass.Classify(transient),
		"permanent": errclass.Classify(permanent),
		"unknown":   errclass.Classify(unknown),
	}
	distinct := map[errclass.Kind]bool{}
	for _, k := range kinds {
		distinct[k] = true
	}
	rec.Add(expkit.Variant{
		Name:    "3値分類（EXP-68）: 3つに分かれる",
		Metrics: map[string]float64{"distinct_kinds": float64(len(distinct))},
		Notes: []string{
			"transient→" + kinds["transient"].String() +
				" / permanent→" + kinds["permanent"].String() +
				" / unknown→" + kinds["unknown"].String() + "（Unknown を恒久と偽らない）",
		},
	})

	// ---- ③ 『恒久以外は retry』の素朴設計は Unknown を無限 retry する ----
	const maxAttempts = 5
	naiveAttempts, _ := retrylab.Do(maxAttempts, 0, func() error { return unknown }) // Naive 相当（下で確認）
	// retrylab.Do は Retryable=false で1回で返る。素朴設計は「恒久以外 retry」なので Unknown を回す:
	naive := errclass.NaiveRetryUnlessKnownPermanent(unknown) // true = 無限 retry してしまう
	safe := errclass.ShouldRetry(errclass.Classify(unknown))  // false = fail-safe
	// 素朴設計での試行回数（Unknown を retry 対象にした場合）
	naiveLoop := simulateRetry(maxAttempts, unknown, errclass.NaiveRetryUnlessKnownPermanent)
	safeLoop := simulateRetry(maxAttempts, unknown, func(e error) bool { return errclass.ShouldRetry(errclass.Classify(e)) })
	_ = naiveAttempts
	rec.Add(expkit.Variant{
		Name:     "Unknown への retry: 素朴『恒久以外 retry』",
		Accident: true,
		Metrics:  map[string]float64{"attempts": float64(naiveLoop), "retries_unknown": b2f(naive)},
		Notes:    []string{"Unknown を Transient 扱いして maxAttempts=" + strconv.Itoa(maxAttempts) + " 回まで無駄に叩く"},
	})
	rec.Add(expkit.Variant{
		Name:    "Unknown への retry: 3値 ShouldRetry（fail-safe）",
		Metrics: map[string]float64{"attempts": float64(safeLoop), "retries_unknown": b2f(safe)},
		Notes:   []string{"Unknown は retry しない（1回で返す）。下流で『不明』として surface する"},
	})

	// ---- 検証 ----
	if len(falseBucket) < 2 {
		t.Errorf("bool 分類で Permanent と Unknown が同居していない（前提が崩れている）: %v", falseBucket)
	}
	if len(distinct) != 3 {
		t.Errorf("3値分類が3つに分かれていない: %v", kinds)
	}
	if errclass.Classify(unknown) != errclass.Unknown {
		t.Errorf("未知エラーが Unknown でない: %v", errclass.Classify(unknown))
	}
	if errclass.Classify(permanent) != errclass.Permanent {
		t.Errorf("重複キーが Permanent でない")
	}
	if errclass.Classify(transient) != errclass.Transient {
		t.Errorf("デッドロックが Transient でない")
	}
	if !naive || safe {
		t.Errorf("素朴=Unknownをretry(true) / 3値=retryしない(false) にならない: naive=%v safe=%v", naive, safe)
	}
	if naiveLoop <= safeLoop {
		t.Errorf("素朴設計が Unknown を余計に叩いていない: naive=%d safe=%d", naiveLoop, safeLoop)
	}

	rec.Scope(
		"エラー値を直接構築して分類（MySQL 1213/1062・未知の error）。DB は不要",
		"3値 = Transient / Permanent / Unknown。retry 判断と下流の扱いを分けるための型",
		"取りこぼし（Kind を足して switch 未更新）は EXP-67 の exhaustive 検査が担保",
	)
	rec.Uncertain(
		"どの MySQL 番号を Permanent とみなすかは運用依存（ここでは代表的な数種）",
		"Unknown の下流処理（surface / 限定 retry / DLQ）は業務要件。ここでは『retry しない＋区別できる』までを示す",
		"実 DB 由来のエラーでの分類網羅は EXP-49（retrylab）側で実測済み",
	)
	rec.Artifact(
		"internal/errclass: Kind(3値)・Classify・ShouldRetry(exhaustive な switch)・Naive(事故側)",
		"EXP-67 exhaustive: Kind を足すと ShouldRetry の未網羅を検出",
	)
	rec.Next("EXP-69 encoding/json/v2 のパースコスト")

	files, err := rec.Save(strings.Join([]string{
		"エラー分類は bool でなく3値(Transient/Permanent/Unknown)の型で持つ。",
		"bool は恒久と不明を潰し、『知らない』を『恒久』と偽る（下流を分けられない・素朴設計は Unknown を無限 retry）。",
		"3値なら retry 判断と下流の扱いを分けられ、Kind の取りこぼしは EXP-67 の exhaustive が拾う。",
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bool false バケツ=%v / 3値=%v / naiveLoop=%d safeLoop=%d / 結果: %v",
		falseBucket, kinds, naiveLoop, safeLoop, files)
}

// simulateRetry は shouldRetry が true の間だけ最大 max 回叩き、試行回数を返す。
func simulateRetry(max int, err error, shouldRetry func(error) bool) int {
	attempts := 0
	for attempts < max {
		attempts++
		if !shouldRetry(err) {
			break
		}
	}
	return attempts
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
