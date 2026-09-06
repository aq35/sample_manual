package appx_test

import (
	"fmt"
	"testing"

	"github.com/samber/lo"

	"github.com/aq35/sample_manual/internal/appx"
)

func TestIf(t *testing.T) {
	if got := appx.If(true, "A", "B"); got != "A" {
		t.Errorf("If true = %q", got)
	}
	if got := appx.If(false, 1, 2); got != 2 {
		t.Errorf("If false = %d", got)
	}
}

func TestIfF_選ばれた側だけ評価する(t *testing.T) {
	calls := map[string]int{}
	got := appx.IfF(true,
		func() string { calls["t"]++; return "t" },
		func() string { calls["f"]++; return "f" })
	if got != "t" || calls["t"] != 1 || calls["f"] != 0 {
		t.Errorf("IfF は選ばれた側だけ呼ぶはず: got=%q calls=%v", got, calls)
	}
}

func TestCoalesce(t *testing.T) {
	if got := appx.Coalesce("", "", "8080"); got != "8080" {
		t.Errorf("Coalesce=%q", got)
	}
	if got := appx.Coalesce(0, 5, 9); got != 5 {
		t.Errorf("Coalesce=%d", got)
	}
}

func TestKeyByID(t *testing.T) {
	type row struct {
		ID   string
		Name string
	}
	rows := []row{{"a", "A"}, {"b", "B"}}
	m := appx.KeyByID(rows, func(r row) string { return r.ID })
	if m["b"].Name != "B" {
		t.Errorf("KeyByID: %v", m)
	}
}

func TestPartition(t *testing.T) {
	yes, no := appx.Partition([]int{1, 2, 3, 4}, func(n int) bool { return n%2 == 0 })
	if fmt.Sprint(yes) != "[2 4]" || fmt.Sprint(no) != "[1 3]" {
		t.Errorf("Partition: yes=%v no=%v", yes, no)
	}
}

func TestChunkIDs(t *testing.T) {
	if got := appx.ChunkIDs([]int{1, 2, 3, 4, 5}, 2); len(got) != 3 {
		t.Errorf("ChunkIDs len=%d", len(got))
	}
	// size<=0 は 1 固まりに倒す
	if got := appx.ChunkIDs([]int{1, 2, 3}, 0); len(got) != 1 {
		t.Errorf("ChunkIDs(0) は 1 固まりのはず: %v", got)
	}
}

// samber/lo をそのまま app 層で使う例（ここは許される層）。
func TestLoを直接使う例(t *testing.T) {
	names := lo.Map([]int{1, 2, 3}, func(n, _ int) string { return fmt.Sprintf("r%d", n) })
	if fmt.Sprint(names) != "[r1 r2 r3]" {
		t.Errorf("lo.Map: %v", names)
	}
	uniq := lo.Uniq([]int{1, 1, 2, 3, 3})
	if len(uniq) != 3 {
		t.Errorf("lo.Uniq: %v", uniq)
	}
}
