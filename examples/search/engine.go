package search

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	graph "github.com/tra4less/go-graph/graph"
)

// Engine 是调用方自己持有的东西：go-graph core 不管热更新，只提供纯函数 Compile。
type Engine struct {
	opts []graph.Option // 随代码发版，每次热更新都用它编译新文档
	cur  atomic.Pointer[graph.Program]

	mu      sync.Mutex
	retired []*graph.Program // 被热更新换下、可能还有后台工作的 Program
}

// NewEngine 用内嵌的 search.yaml 启动；配置中心的版本到了再调 Reload。
func NewEngine(d Deps) (*Engine, error) {
	opts, err := Options(d)
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: opts}
	if err := e.Reload(defaultDoc); err != nil {
		return nil, err
	}
	return e, nil
}

// Reload 编译新拓扑并原子替换。编译失败返回 *graph.CompileError，旧拓扑继续服务；
// 正在跑的请求不受影响（它们持有旧 Program）。
// 在配置中心的变更回调里调用它即可。
func (e *Engine) Reload(src []byte) error {
	p, err := graph.Compile(src, e.opts...)
	if err != nil {
		return err
	}
	if old := e.cur.Swap(p); old != nil {
		// 旧 Program 上超时被放弃的工作可能还在跑，Drain 时要一起等。
		e.mu.Lock()
		e.retired = append(dropIdle(e.retired), old)
		e.mu.Unlock()
	}
	return nil
}

// Search 跑一次 search 图。出错时返回 *graph.TaskError，Path 指出失败在哪一步。
func (e *Engine) Search(ctx context.Context, uid, query string) ([]Result, error) {
	resp, err := graph.Run(ctx, e.cur.Load(), Search, Request{UID: uid, Query: query})
	return resp.Results, err
}

// Drain 等当前和所有被换下的 Program 起的 goroutine 退出（超时后被放弃、不看 ctx 的工作等），
// 停机前调用。
func (e *Engine) Drain(ctx context.Context) error {
	e.mu.Lock()
	progs := append([]*graph.Program{e.cur.Load()}, e.retired...)
	e.mu.Unlock()
	for _, p := range progs {
		if err := p.Drain(ctx); err != nil {
			return err
		}
	}
	return nil
}

// dropIdle 去掉已经没有后台工作的 Program，免得 retired 随热更新次数一直变长。
// 传给 Drain 一个已取消的 ctx 就是不阻塞的探测：空闲时返回 nil。
func dropIdle(ps []*graph.Program) []*graph.Program {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	return slices.DeleteFunc(ps, func(p *graph.Program) bool { return p.Drain(done) == nil })
}
