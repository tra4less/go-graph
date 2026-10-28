package search

import (
	_ "embed"
	"errors"

	graph "github.com/tra4less/go-graph/graph"
	"github.com/tra4less/go-graph/ext/retry"
)

// Deps 是有外部依赖的算子和中间件的实例，由调用方构造。
type Deps struct {
	Details *Details
	Logger  *Logger
	Metrics *Metrics
}

// Options 返回编译 search.yaml 所需的全部选项：入口、算子、中间件，以及给每个 call 任务
// 套上 metrics 的 WrapCalls。启动时构造一次，每次热更新都用它编译新文档。
//
// 方法值绑定 nil 接收者不会马上 panic，要到算子被调用时才 panic，所以这里先检查。
func Options(d Deps) ([]graph.Option, error) {
	if d.Details == nil || d.Logger == nil || d.Metrics == nil {
		return nil, errors.New("search: Deps needs Details, Logger and Metrics")
	}
	return []graph.Option{
		Search,
		graph.Op("parse_query", ParseQuery),
		graph.Op("recall", Recall),
		graph.Op("load_profile", LoadProfile),
		graph.Op("merge", Merge),
		graph.Op("rerank", Rerank),
		graph.Op("personalize", Personalize),
		graph.Op("sort_by_score", SortByScore),
		graph.Op("fill_detail", d.Details.Fill),
		graph.Op("search_log", d.Logger.SearchLog),
		graph.Middleware("retry", retry.Retry), // 通用重试中间件，见 ext/retry
		graph.Middleware("metrics", d.Metrics.Timing),
		graph.WrapCalls("metrics"),
	}, nil
}

// DefaultDoc 返回内嵌的默认拓扑（测试里拿它配合替身选项编译）。
func DefaultDoc() []byte { return defaultDoc }

//go:embed search.yaml
var defaultDoc []byte
