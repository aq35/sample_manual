package gql

import (
	"context"
	"time"

	"github.com/vikstrous/dataloadgen"

	"github.com/aq35/sample_manual/internal/repo"
)

// loaderMax は1台あたりバッチで取る命令の上限。フィールドの first はこの範囲で切る。
const loaderMax = 100

// Loaders は1リクエストぶんの DataLoader 束。
//
// ★重要（セキュリティ・EXP-24）: ローダは「リクエストのテナントに束縛された Scope」で作る。
// だからキーが robot_id だけでも、バッチは必ずそのテナント内に閉じる。ローダをリクエスト跨ぎで
// 共有したり、テナント非依存のキャッシュにすると、他テナントの結果が混ざりうる。共有しないこと。
type Loaders struct {
	Commands *dataloadgen.Loader[string, []Command]
}

type loadersKey struct{}

// newLoaders は Scope（テナント束縛済み）から、そのテナント専用のローダを作る。
func newLoaders(sc *repo.Scope) *Loaders {
	fetch := func(ctx context.Context, robotIDs []string) ([][]Command, []error) {
		byRobot, err := commandsForRobots(ctx, sc, robotIDs, loaderMax)
		if err != nil {
			errs := make([]error, len(robotIDs))
			for i := range errs {
				errs[i] = err
			}
			return make([][]Command, len(robotIDs)), errs
		}
		out := make([][]Command, len(robotIDs))
		for i, id := range robotIDs {
			out[i] = byRobot[id] // 命令が無ければ nil（それが正しい答え）
		}
		return out, nil
	}
	return &Loaders{
		Commands: dataloadgen.NewLoader(fetch, dataloadgen.WithWait(time.Millisecond)),
	}
}

// WithLoaders はローダを context に載せる（リクエストごとにミドルウェアが呼ぶ）。
func WithLoaders(ctx context.Context, l *Loaders) context.Context {
	return context.WithValue(ctx, loadersKey{}, l)
}

// loadersFrom は context のローダを返す。無ければ (nil, false)＝素朴経路（N+1）にフォールバック。
func loadersFrom(ctx context.Context) (*Loaders, bool) {
	l, ok := ctx.Value(loadersKey{}).(*Loaders)
	return l, ok
}
