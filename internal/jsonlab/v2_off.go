//go:build !goexperiment.jsonv2

package jsonlab

import "errors"

// JSONV2Available は encoding/json/v2 が有効か（GOEXPERIMENT=jsonv2 でビルドされたか）。
const JSONV2Available = false

// ParseStructV2 は v2 未有効時のスタブ（呼ぶとエラー）。テストは skip する。
func ParseStructV2(b []byte) (int, error) {
	return 0, errors.New("encoding/json/v2 未有効: GOEXPERIMENT=jsonv2 でビルドすること")
}
