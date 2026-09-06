package app

// app 層は lo を使ってよい（指摘されない）。
import "github.com/samber/lo"

func Use() []string {
	return lo.Map([]int{1, 2}, func(n, _ int) string { return "x" })
}
