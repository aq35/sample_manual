package model_test

// §8「比較処理（マップ参照＋==、1回あたり）」の再現。
//
//	go test ./internal/model/ -bench Compare -benchmem -count=3
//
// 結論の使いどころ: 整数型の速度差は誤差（1件あたり数 ns）。
// 整数型を選ぶ理由は「速いから」ではなく「打ち間違えられないから」（§6.3）。

import (
	"fmt"
	"testing"

	"github.com/aq35/sample_manual/internal/model"
)

const n = 1000

type stringState struct {
	Status  string
	Online  bool
	Battery int8
}

var (
	uintMap   = make(map[model.ID]model.State, n)
	strMap    = make(map[model.ID]stringState, n)
	worstMap  = make(map[model.ID]stringState, n)
	ids       = make([]model.ID, 0, n)
	uintProbe model.State
	strProbe  stringState
	worstA    = "status_aaaaaaaaaaaaaaaaaaaa"
	worstB    = "status_aaaaaaaaaaaaaaaaaaab" // 同長・末尾1文字違い＝最悪ケース
	sink      bool
)

func init() {
	for i := 0; i < n; i++ {
		id := model.ID(fmt.Sprintf("r%05d", i))
		ids = append(ids, id)
		uintMap[id] = model.State{Status: model.StatusRunning, Online: true, Battery: 80}
		strMap[id] = stringState{Status: "running", Online: true, Battery: 80}
		worstMap[id] = stringState{Status: worstA, Online: true, Battery: 80}
	}
	uintProbe = model.State{Status: model.StatusRunning, Online: true, Battery: 80}
	strProbe = stringState{Status: "running", Online: true, Battery: 80}
}

func BenchmarkCompare_StatusUint8(b *testing.B) {
	i := 0
	for b.Loop() {
		id := ids[i%n]
		i++
		sink = uintMap[id] == uintProbe
	}
}

func BenchmarkCompare_StatusString(b *testing.B) {
	i := 0
	for b.Loop() {
		id := ids[i%n]
		i++
		sink = strMap[id] == strProbe
	}
}

// 同じ長さで末尾だけ違う文字列は、比較が最後まで走る最悪ケース。
func BenchmarkCompare_StatusStringWorstCase(b *testing.B) {
	probe := stringState{Status: worstB, Online: true, Battery: 80}
	i := 0
	for b.Loop() {
		id := ids[i%n]
		i++
		sink = worstMap[id] == probe
	}
}
