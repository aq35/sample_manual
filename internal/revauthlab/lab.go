// Package revauthlab は EXP-53（長寿命接続の途中失権）の実験本体。
//
// SSE/subscription は何時間も生きる。接続時に一度だけ認可すると、その後トークンが失効・権限が
// 剥奪されても配信が続いてしまう（情報漏洩）。配信ループの中で定期的に再認可(re-auth)し、
// 失権を検知したら購読を切る。「接続時だけ」と「定期 re-auth」で、失権後に届く件数を比べる。
package revauthlab

// Authorizer は「この購読者は今も購読してよいか」を返す（grant 表・トークン検証の抽象）。
type Authorizer interface {
	Authorized(subID string) bool
}

// GrantStore は購読者→許可の可変な集合（EXP-28 の grant 表相当）。Revoke で剥奪する。
type GrantStore struct {
	granted map[string]bool
}

func NewGrantStore(subIDs ...string) *GrantStore {
	g := &GrantStore{granted: map[string]bool{}}
	for _, id := range subIDs {
		g.granted[id] = true
	}
	return g
}

func (g *GrantStore) Authorized(subID string) bool { return g.granted[subID] }
func (g *GrantStore) Revoke(subID string)          { g.granted[subID] = false }

// Deliver は event ごとに配信を試みる消費ループのモデル。
//
//	reauthEvery: 何 event ごとに再認可するか（0 以下＝接続時だけ＝再認可しない）。
//	events:      流れてくる event 総数。
//	revokeAt:    この event の直前に grant を剥奪する（-1 なら剥奪しない）。
//
// 返り値:
//
//	delivered:      配信できた総数
//	deliveredAfter: 剥奪後に配信してしまった数（漏洩＝小さいほど良い、理想 0）
//	stopLatency:    剥奪から配信停止までに流れた event 数（再認可の粒度で決まる）
func Deliver(auth Authorizer, store *GrantStore, subID string, reauthEvery, events, revokeAt int) (delivered, deliveredAfter, stopLatency int) {
	authorized := auth.Authorized(subID) // 接続時の認可
	revoked := false
	stopLatency = -1

	for i := 0; i < events; i++ {
		// 剥奪イベント（この event の配信前に権限が剥奪される）
		if revokeAt >= 0 && i == revokeAt {
			store.Revoke(subID)
			revoked = true
		}
		// 定期 re-auth: reauthEvery ごとに認可を取り直す
		if reauthEvery > 0 && i%reauthEvery == 0 {
			authorized = auth.Authorized(subID)
		}
		if !authorized {
			if revoked && stopLatency < 0 {
				stopLatency = i - revokeAt // 剥奪から止まるまでに流れた数
			}
			continue // 配信しない
		}
		delivered++
		if revoked {
			deliveredAfter++
		}
	}
	if revoked && stopLatency < 0 {
		// 最後まで止まらなかった（接続時だけ認可のケース）
		stopLatency = events - revokeAt
	}
	return delivered, deliveredAfter, stopLatency
}
