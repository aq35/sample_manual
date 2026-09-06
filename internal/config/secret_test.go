package config_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aq35/sample_manual/internal/config"
)

func TestRequiredSecret_取得できなければfailfast(t *testing.T) {
	l := config.New()
	fail := func(ctx context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("SecretManager 落ちてる")
	}
	_ = l.RequiredSecret("DB_PASSWORD", fail, time.Second)
	if !errors.Is(l.Err(), config.ErrMissingConfig) {
		t.Errorf("秘密が取れないのに fail-fast しない: %v", l.Err())
	}
}

func TestRequiredSecret_取得と期限(t *testing.T) {
	l := config.New()
	ok := func(ctx context.Context) (string, time.Time, error) {
		return "pw-v1", time.Now().Add(time.Hour), nil
	}
	s := l.RequiredSecret("DB_PASSWORD", ok, time.Second)
	if l.Err() != nil {
		t.Fatalf("成功のはず: %v", l.Err())
	}
	if s.Value() != "pw-v1" || s.Expired() {
		t.Errorf("value=%q expired=%v", s.Value(), s.Expired())
	}
	// Audit に値は出さない
	for _, a := range l.Audit() {
		if a == "DB_PASSWORD=secret(env外)" {
			return
		}
	}
	t.Errorf("Audit に秘密の出所が無い: %v", l.Audit())
}

func TestManagedSecret_期限前に先回り更新しonRotate(t *testing.T) {
	var ver atomic.Int64
	fetch := func(ctx context.Context) (string, time.Time, error) {
		n := ver.Add(1)
		// すぐ切れる期限にして、先回り更新をすぐ起こす
		return "pw-v" + itoa(n), time.Now().Add(80 * time.Millisecond), nil
	}
	l := config.New()
	s := l.RequiredSecret("DB_PASSWORD", fetch, 40*time.Millisecond)
	if l.Err() != nil {
		t.Fatal(l.Err())
	}
	rotated := make(chan string, 4)
	s.Start(func(v string) { rotated <- v })
	defer s.Stop()

	select {
	case v := <-rotated:
		if v == "pw-v1" {
			t.Errorf("更新後の値が初期値のまま: %q", v)
		}
		t.Logf("先回り更新で onRotate が呼ばれた: %q", v)
	case <-time.After(2 * time.Second):
		t.Fatal("先回り更新が起きない")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
