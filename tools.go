//go:build tools

// Package tools は生成ツールを依存として固定するためだけのもの（ビルドには入らない）。
//
//	go run github.com/99designs/gqlgen generate
package tools

import _ "github.com/99designs/gqlgen"
