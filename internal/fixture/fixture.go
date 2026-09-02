// Package fixture は、ベンチマークで使う API 応答（JSON）を再現可能な形で作る。
//
// §8 の測定条件に合わせて「1,000件・約 336 KB」の応答を生成する。
// 乱数の種を固定しているので、何度実行しても同じバイト列になる。
package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/aq35/sample_manual/internal/jsonx"
)

// RobotsJSON は N 件ぶんの一覧応答を返す。
// API は20項目返すが、こちらが要るのは5項目だけ、という §6.1 の状況を再現する。
func RobotsJSON(n int) []byte {
	rnd := rand.New(rand.NewSource(20260902))
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	statuses := []string{"running", "stopped", "error"}

	robots := make([]jsonx.FatRobot, 0, n)
	for i := 0; i < n; i++ {
		robots = append(robots, jsonx.FatRobot{
			ID:           fmt.Sprintf("r%05d", i),
			Status:       statuses[rnd.Intn(len(statuses))],
			Online:       rnd.Intn(10) != 0,
			Batt:         round(rnd.Float64()*100, 1),
			ObservedAt:   base.Add(time.Duration(rnd.Intn(60)) * time.Second).Format(time.RFC3339),
			Name:         fmt.Sprintf("rb%05d", i),
			Model:        "AGV-3000",
			Firmware:     "2.14.3",
			Serial:       fmt.Sprintf("SN%06d", i),
			Site:         fmt.Sprintf("site-%02d", i%20),
			Zone:         fmt.Sprintf("zone-%c", 'A'+rune(i%6)),
			IPAddress:    fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256),
			MACAddress:   fmt.Sprintf("02:42:ac:%02x:%02x:%02x", i/65536%256, i/256%256, i%256),
			Uptime:       int64(rnd.Intn(1_000_000)),
			ErrorCode:    "",
			ErrorMessage: "",
			TaskID:       fmt.Sprintf("t%06d", rnd.Intn(1_000_000)),
			TaskProgress: round(rnd.Float64(), 2),
			Temperature:  round(20+rnd.Float64()*15, 1),
			Humidity:     round(30+rnd.Float64()*40, 1),
			Odometer:     round(rnd.Float64()*1_000_000, 1),
			LastMaint:    base.AddDate(0, 0, -rnd.Intn(300)).Format(time.RFC3339),
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(jsonx.FatResponse{Robots: robots}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// OneRobotJSON は WebSocket で1件ずつ届く形（§6.2 の3つめ）。
func OneRobotJSON() []byte {
	all := RobotsJSON(1)
	var resp jsonx.FatResponse
	if err := json.Unmarshal(all, &resp); err != nil {
		panic(err)
	}
	b, err := json.Marshal(resp.Robots[0])
	if err != nil {
		panic(err)
	}
	return b
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
