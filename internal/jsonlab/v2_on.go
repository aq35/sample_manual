//go:build goexperiment.jsonv2

package jsonlab

import jsonv2 "encoding/json/v2"

// JSONV2Available は encoding/json/v2 が有効か（GOEXPERIMENT=jsonv2 でビルドされたか）。
const JSONV2Available = true

// ParseStructV2 は encoding/json/v2 で []Item に展開する。
func ParseStructV2(b []byte) (int, error) {
	var xs []Item
	if err := jsonv2.Unmarshal(b, &xs); err != nil {
		return 0, err
	}
	return len(xs), nil
}
