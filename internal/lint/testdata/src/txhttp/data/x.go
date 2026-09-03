package data

import (
	"context"
	"net/http"
)

type runner struct{ client *http.Client }

func (r *runner) Tx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func (r *runner) Work(ctx context.Context) error {
	return r.Tx(ctx, func(ctx context.Context) error {
		resp, err := r.client.Get("http://example.com") // want "トランザクションの中で外部を呼んでいる"
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})
}
