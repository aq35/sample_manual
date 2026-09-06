package mo

type IO[R any] struct{ f func() R }

func NewIO[R any](f func() R) IO[R] { return IO[R]{f} }
func (io IO[R]) Run() R             { return io.f() }
