package expkit

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
)

// 子プロセス側で使う故障注入。
//
// 外から時間を見計らって kill すると、どの地点で落ちたかが実行ごとに変わる。
// **プロセス自身が「この地点で死ぬ」と決める**ほうが、再現性のある実験になる。
// SIGKILL は捕捉できないので、自分で送っても外から送っても意味は同じ。

// KillPointEnv は「この地点で自分を SIGKILL する」地点名を渡す環境変数。
const KillPointEnv = "EXP_KILL_AT"

// PausePointEnv は「この地点で止まって合図を出す」地点名を渡す環境変数。
// 外から SIGTERM などを正確な位置で送りたいとき（graceful shutdown の実験）に使う。
const PausePointEnv = "EXP_PAUSE_AT"

// MarkerPrefix は子プロセスが親に地点を知らせる行の接頭辞。
const MarkerPrefix = "EXPPOINT "

// KillSwitch は子プロセスの中に置く故障注入器。
type KillSwitch struct {
	killAt  string
	pauseAt string

	mu      sync.Mutex
	reached []string
	paused  chan struct{}
}

// NewKillSwitch は環境変数から設定を読む。
func NewKillSwitch() *KillSwitch {
	return &KillSwitch{
		killAt:  os.Getenv(KillPointEnv),
		pauseAt: os.Getenv(PausePointEnv),
		paused:  make(chan struct{}),
	}
}

// Point はプログラム中の名前つき地点。
//
//   - EXP_KILL_AT と一致すれば、その場で **SIGKILL**（回復処理も defer も走らない）
//   - EXP_PAUSE_AT と一致すれば、印を出して止まる（外から信号を送るため）
//   - どちらでもなければ、通過した記録だけ残して先へ進む
func (k *KillSwitch) Point(name string) {
	k.mu.Lock()
	k.reached = append(k.reached, name)
	k.mu.Unlock()

	// 親が「どこまで進んだか」を読めるように、必ず印を出す
	fmt.Printf("%s%s\n", MarkerPrefix, name)
	_ = os.Stdout.Sync()

	if k.killAt != "" && k.killAt == name {
		fmt.Printf("%sKILLING at %s\n", MarkerPrefix, name)
		_ = os.Stdout.Sync()
		// ★自分に SIGKILL。defer も回復処理も走らない
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // 念のため（ここには到達しない）
	}
	if k.pauseAt != "" && k.pauseAt == name {
		fmt.Printf("%sPAUSED at %s\n", MarkerPrefix, name)
		_ = os.Stdout.Sync()
		<-k.paused // Resume されるまで止まる
	}
}

// Resume は停止中の地点を解除する（子プロセス内から呼ぶ）。
func (k *KillSwitch) Resume() {
	select {
	case <-k.paused:
	default:
		close(k.paused)
	}
}

// Reached は通過した地点の一覧。
func (k *KillSwitch) Reached() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.reached...)
}

// KillPoints は地点名の一覧を定義するための型。
// 実験ごとに定数で持ち、テスト側が「全地点で1回ずつ殺す」を回せるようにする。
type KillPoints []string

func (p KillPoints) Contains(name string) bool {
	for _, n := range p {
		if n == name {
			return true
		}
	}
	return false
}

func (p KillPoints) String() string { return strings.Join(p, ", ") }
