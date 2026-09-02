package fixture

import "testing"

// TestPayloadSize は §8 の測定条件「1,000件・336 KB」を再現できているか確認する。
func TestPayloadSize(t *testing.T) {
	b := RobotsJSON(1000)
	t.Logf("1,000件の応答: %d バイト (%.1f KB)", len(b), float64(len(b))/1024)
	t.Logf("WebSocket 1件: %d バイト", len(OneRobotJSON()))
	if len(b) < 200*1024 || len(b) > 500*1024 {
		t.Fatalf("想定した大きさ (200〜500KB) から外れている: %d", len(b))
	}
}
