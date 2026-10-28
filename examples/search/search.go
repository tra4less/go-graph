// Package search 是 go-graph 的完整使用示例：一次搜索请求
// 「解析 → 多路召回 → 合并 → 精排 → 按用户分流排序 → 补详情」。
//
// 文件分工：
//   - search.go      入口 Search 和领域类型（Request、Doc、Profile 等）
//   - ops.go         算子与中间件的实现（随代码发版）
//   - options.go     把入口、算子、中间件交给 Compile 的选项，以及默认拓扑 DefaultDoc（随代码发版）
//   - search.yaml    拓扑（可热更新）
//   - engine.go      调用方持有的引擎：编译、热更新、执行、停机
//
// 同一份选项也可以从 YAML 声明生成，见 examples/searchgen。
package search

import graph "github.com/tra4less/go-graph/graph"

// Request 是 search 图的入参：两个字段都不是 Maybe，所以整张图里 uid、query 都视为已写入。
type Request struct {
	UID   string `graph:"uid"`
	Query string `graph:"query"`
}

// Response 是 search 图的出参：Compile 检查图结束时 results 一定已写入。
type Response struct {
	Results []Result `graph:"results"`
}

// Search 是 search 图的入口。文档里没有 Entry 的图（default_rank）是子图，只能被 run 引用。
var Search = graph.NewEntry[Request, Response]("search")

// Query 是解析后的查询。
type Query struct{ Terms []string }

// Doc 是召回、排序阶段流转的文档。
type Doc struct {
	ID    string
	Title string
	Score float64
}

// Result 是补完详情、返回给调用方的结果。
type Result struct {
	ID, Title, Detail string
}

// Profile 是用户画像。CEL 里写 profile.HasHistory。
type Profile struct {
	HasHistory bool
	Likes      []string // 用户偏好的词
}
