package expkit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 親（テスト）側から子プロセスを操るための道具。
//
// crash の実験は「プロセスが本当に死ぬ」ことが要点なので、goroutine を止める
// 模擬ではなく、実際に別プロセスを起動して SIGKILL する。

// Build は指定パッケージをビルドして実行ファイルの場所を返す。
// テストごとにビルドし直さないよう、同じパッケージなら使い回す。
func Build(t testing.TB, pkg string) string {
	t.Helper()
	buildOnce.Do(func() { buildCache = map[string]string{} })

	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := buildCache[pkg]; ok {
		return p
	}
	dir, err := os.MkdirTemp("", "expkit-bin-")
	if err != nil {
		t.Fatalf("一時ディレクトリを作れない: %v", err)
	}
	bin := filepath.Join(dir, "child")
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("子プロセスのビルドに失敗: %v\n%s", err, out)
	}
	buildCache[pkg] = bin
	return bin
}

var (
	buildOnce  sync.Once
	buildMu    sync.Mutex
	buildCache map[string]string
)

// Child は起動中の子プロセス。
type Child struct {
	cmd    *exec.Cmd
	stdout *lineReader
	stderr *lineReader

	mu       sync.Mutex
	exited   bool
	exitInfo ExitInfo
	waitOnce sync.Once
	waitErr  error
}

// ExitInfo は子プロセスの終わり方。
type ExitInfo struct {
	Code     int            // 終了コード（信号で死んだ場合は -1）
	Signal   syscall.Signal // 信号で死んだ場合のその信号
	Killed   bool           // 信号で死んだか
	Duration time.Duration
	Stdout   string
	Stderr   string
	Points   []string // 子が通過した地点（EXPPOINT 行）
}

func (e ExitInfo) String() string {
	if e.Killed {
		return fmt.Sprintf("signal=%v duration=%v", e.Signal, e.Duration.Round(time.Millisecond))
	}
	return fmt.Sprintf("exit=%d duration=%v", e.Code, e.Duration.Round(time.Millisecond))
}

// StartChild は子プロセスを起動する。extraEnv は "K=V" の形。
func StartChild(ctx context.Context, bin string, args []string, extraEnv ...string) (*Child, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	// 子プロセスを独立したプロセスグループにする。
	// こうしないと、テストプロセスへの信号が子にも飛んでしまう。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	c := &Child{cmd: cmd}
	c.stdout = newLineReader(stdout)
	c.stderr = newLineReader(stderr)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c.exitInfo.Duration = 0
	go func() { _ = start }()
	return c, nil
}

// PID は子プロセスの PID。
func (c *Child) PID() int { return c.cmd.Process.Pid }

// WaitLine は接頭辞にあてはまる行が来るまで待つ。
func (c *Child) WaitLine(prefix string, timeout time.Duration) (string, error) {
	return c.stdout.waitFor(prefix, timeout)
}

// WaitPoint は子が指定の地点に達するまで待つ（EXPPOINT 行）。
func (c *Child) WaitPoint(name string, timeout time.Duration) error {
	_, err := c.WaitLine(MarkerPrefix+name, timeout)
	return err
}

// Signal は子プロセスに信号を送る。
func (c *Child) Signal(sig syscall.Signal) error {
	return syscall.Kill(c.cmd.Process.Pid, sig)
}

// Kill は SIGKILL を送る。
func (c *Child) Kill() error { return c.Signal(syscall.SIGKILL) }

// Wait は終了を待つ。timeout を超えたら SIGKILL して待つ。
func (c *Child) Wait(timeout time.Duration) (ExitInfo, error) {
	start := time.Now()
	done := make(chan error, 1)
	c.waitOnce.Do(func() {
		go func() { done <- c.cmd.Wait() }()
	})

	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		_ = c.Kill()
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			return ExitInfo{}, errors.New("子プロセスが終了しない")
		}
	}

	info := ExitInfo{Duration: time.Since(start), Code: -1}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		info.Code = 0
	case errors.As(err, &exitErr):
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if ok && status.Signaled() {
			info.Killed = true
			info.Signal = status.Signal()
		} else {
			info.Code = exitErr.ExitCode()
		}
	default:
		return ExitInfo{}, err
	}
	info.Stdout = c.stdout.text()
	info.Stderr = c.stderr.text()
	info.Points = pointsFrom(info.Stdout)

	c.mu.Lock()
	c.exited = true
	c.exitInfo = info
	c.mu.Unlock()
	return info, nil
}

func pointsFrom(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, MarkerPrefix) {
			out = append(out, strings.TrimPrefix(strings.TrimSpace(line), MarkerPrefix))
		}
	}
	return out
}

// lineReader は子プロセスの出力を行単位で溜め、待ち合わせを可能にする。
type lineReader struct {
	mu      sync.Mutex
	cond    *sync.Cond
	lines   []string
	done    bool
	scanErr error
}

func newLineReader(r io.Reader) *lineReader {
	lr := &lineReader{}
	lr.cond = sync.NewCond(&lr.mu)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lr.mu.Lock()
			lr.lines = append(lr.lines, sc.Text())
			lr.cond.Broadcast()
			lr.mu.Unlock()
		}
		lr.mu.Lock()
		lr.done = true
		lr.scanErr = sc.Err()
		lr.cond.Broadcast()
		lr.mu.Unlock()
	}()
	return lr
}

func (lr *lineReader) waitFor(prefix string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	// Cond にはタイムアウトが無いので、定期的に起こす
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				lr.cond.Broadcast()
			case <-stop:
				return
			}
		}
	}()

	lr.mu.Lock()
	defer lr.mu.Unlock()
	seen := 0
	for {
		for ; seen < len(lr.lines); seen++ {
			if strings.HasPrefix(lr.lines[seen], prefix) {
				return lr.lines[seen], nil
			}
		}
		if lr.done {
			return "", fmt.Errorf("子プロセスの出力が終わった（%q は現れなかった）", prefix)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%v 以内に %q が現れなかった", timeout, prefix)
		}
		lr.cond.Wait()
	}
}

func (lr *lineReader) text() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return strings.Join(lr.lines, "\n")
}
