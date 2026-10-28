// Package searchgen 演示可选工具 graphgen：从 operators.yaml 生成 examples/search 的注册选项，
// 代替手写的 search.Options。生成的 Options 会检查方法接收者，一次列出所有为 nil 的。
//
// WrapCalls 这类 Program 级设置不在 operators.yaml 里声明，调用方把它追加到生成的选项后面。
package searchgen

//go:generate go run github.com/tra4less/go-graph/cmd/graphgen -in operators.yaml -out operators_gen.go
