package domain

import "time"

func Deadline() time.Time {
	return time.Now().Add(time.Hour) // want "time.Now\\(\\) を直に呼んでいる"
}

// 逃げ道（理由つき）は通す
//
//smlint:allow domaintime 理由: 移行中。2026-12 までに時計を注入する
func LegacyNow() time.Time {
	return time.Now()
}
