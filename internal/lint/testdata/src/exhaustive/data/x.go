package data

// Status は enum 的な named 整数型（定数が2つ以上 → enum とみなす）。
type Status int

const (
	Pending Status = iota
	InProgress
	Completed
)

// 網羅していない（Completed を忘れている・default も無い）→ 検出する。
func handleBad(s Status) string {
	switch s { // want "網羅していない（未処理: Completed）"
	case Pending:
		return "p"
	case InProgress:
		return "ip"
	}
	return ""
}

// 全メンバを網羅 → 検出しない。
func handleGood(s Status) string {
	switch s {
	case Pending:
		return "p"
	case InProgress:
		return "ip"
	case Completed:
		return "c"
	}
	return ""
}

// default がある → 「残りはまとめて」と意図した扱い。検出しない。
func handleDefault(s Status) string {
	switch s {
	case Pending:
		return "p"
	default:
		return "other"
	}
}

// 逃げ道（理由つき）は通す。
//
//smlint:allow exhaustive 理由: 移行中。Completed は次のPRで対応する
func handleAllowed(s Status) string {
	switch s {
	case Pending:
		return "p"
	case InProgress:
		return "ip"
	}
	return ""
}
