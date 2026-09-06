// Package moio_test は「samber/mo の IO を repository 層に入れない方がいい理由」を
// 文章ではなく**動くコード**で示す。
//
//	go test ./internal/moio/ -run TestMoIO -v
//
// mo.IO は「副作用を遅延した値」として包む関数型のイディオム。
// app 層で純粋なロジックを組み立てるぶんには使える。だが DB 境界に置くと、
// この repo が EXP-1/EXP-2 で苦労して守っている性質（error を捨てない・
// context を通す・OUTCOME_UNKNOWN を区別する）を覆い隠す。
package moio_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samber/mo"
)

// ---- 比較対象: この repo の流儀（context + 明示的な error）----

var errDB = errors.New("DB エラー")

// getName は「素の Go」。ctx を通し、(値, error) を返す。
func getName(ctx context.Context, fail bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err // ★キャンセル/タイムアウトが呼び出し側に届く
	}
	if fail {
		return "", errDB
	}
	return "ロビー", nil
}

// ---- ① mo.IO は error を Run() の戻りから消す ----

func TestMoIO_Runはerrorを返さない(t *testing.T) {
	// mo.IO[R].Run() の戻りは R だけ。失敗をどう表すかが型に出ない。
	io := mo.NewIO(func() string {
		_, err := getName(context.Background(), true) // 失敗する
		if err != nil {
			// ここで握った error を、Run() の戻り値では表せない。
			// ゼロ値を返す＝「失敗」と「空文字が正しい結果」が区別できなくなる。
			return ""
		}
		return "ロビー"
	})
	got := io.Run() // 戻りは string のみ。error は無い。
	if got != "" {
		t.Fatalf("got=%q", got)
	}
	// ★ここが問題: got=="" が「エラーだった」のか「本当に空だった」のか、呼び出し側に分からない。
	t.Log("mo.IO.Run() は R だけを返す → error が型から消え、ゼロ値に潰れる")
}

// ---- ② IOEither にすると error は戻るが、(R,error)+ctx から離れる ----

func TestMoIO_IOEitherでも素のGoから離れる(t *testing.T) {
	io := mo.NewIOEither(func() (string, error) {
		return getName(context.Background(), true)
	})
	res := io.Run() // Either[error, string]
	if res.IsRight() {
		t.Fatal("失敗のはずが Right")
	}
	// error は取れる。だが:
	//  - 呼び出し規約が (R, error) から Either[error,R] に変わり、標準ライブラリや
	//    database/sql と噛み合わない（毎回 変換が要る）
	//  - errors.Is / errors.As での分類（ErrNotFound 等）に一手間増える
	leftErr, _ := res.Left()
	if !errors.Is(leftErr, errDB) {
		t.Errorf("error が errDB でない: %v", leftErr)
	}
	t.Log("IOEither は error を運べるが、database/sql の (R,error) 規約から離れ、変換が要る")
}

// ---- ③ context を通さない: キャンセル/タイムアウトが遅延の中に埋もれる ----

func TestMoIO_contextが遅延に埋もれる(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// IO を「組み立てる」時点と「Run する」時点がずれるのが mo.IO の売り。
	// だが DB 呼び出しでは、その間に ctx がキャンセルされうる。
	io := mo.NewIO(func() string {
		v, err := getName(ctx, false)
		if err != nil {
			return "<canceled>"
		}
		return v
	})

	cancel() // 組み立て後・Run 前にキャンセル
	got := io.Run()

	// 素の Go なら getName(ctx,...) が ctx.Err() を返し、呼び出し側が即座に気づく。
	// mo.IO では「いつ Run されるか」が型に出ないので、キャンセルの扱いが遅延の内側に隠れる。
	if got != "<canceled>" {
		t.Errorf("got=%q（ctx キャンセルが内側に埋もれた）", got)
	}
	t.Log("Run の時点が型に出ない → ctx キャンセル/期限の扱いが遅延の内側に隠れる")
}

// ---- ④ EXP-1 の核: 「effect は起きたが結果が不明」を潰す ----

func TestMoIO_OUTCOME_UNKNOWNを潰す(t *testing.T) {
	// EXP-1 の教訓: 外部 effect の途中でタイムアウトしたら、それは「effect なし」ではない。
	// 素の Go は ctx.DeadlineExceeded を返し、呼び出し側は OUTCOME_UNKNOWN として扱える。
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // 期限を確実に過ぎさせる

	_, err := getName(ctx, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("素の Go は期限超過を返すはず: %v", err)
	}

	// これを mo.IO[string] で包むと、Run() の戻りは string だけ。
	// タイムアウト＝OUTCOME_UNKNOWN を、ゼロ値（=「結果は空」）と同じ形に潰してしまう。
	io := mo.NewIO(func() string {
		v, err := getName(ctx, false)
		if err != nil {
			return "" // ★UNKNOWN が「空の結果」に化ける
		}
		return v
	})
	if got := io.Run(); got != "" {
		t.Fatalf("got=%q", got)
	}
	t.Log("mo.IO は timeout(OUTCOME_UNKNOWN) をゼロ値に潰す → EXP-1 の禁止事項に抵触")
}

// ---- どこでなら使えるか ----

func TestMoIO_app層の純粋な組み立てなら可(t *testing.T) {
	// 副作用のない値の遅延・合成なら、意味を潰す相手（DB の error/ctx）が居ないので問題ない。
	price := mo.NewIO(func() int { return 100 })
	taxed := mo.NewIO(func() int { return price.Run() * 110 / 100 })
	if got := taxed.Run(); got != 110 {
		t.Errorf("got=%d", got)
	}
	t.Log("DB 境界の外（純粋な組み立て）なら mo.IO も可。境界に置かないのが要点")
}
