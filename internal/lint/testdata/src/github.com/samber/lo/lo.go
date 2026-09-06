package lo

func Sum[T int | float64](xs []T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}
func Map[T, R any](xs []T, f func(T, int) R) []R {
	out := make([]R, len(xs))
	for i, x := range xs {
		out[i] = f(x, i)
	}
	return out
}
