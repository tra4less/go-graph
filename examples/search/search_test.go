package search_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tra4less/go-graph/examples/search"
	graph "github.com/tra4less/go-graph/graph"
)

func Example() {
	var mu sync.Mutex
	var logs []string
	calls := map[string][2]int{} // 算子 → {调用次数, 失败次数}
	ran := map[string]error{}    // 本次请求里跑过的任务路径 → 算子的错误
	flaky := true

	eng, err := search.NewEngine(search.Deps{
		// 详情服务第一次调用抖动，靠 retry 中间件重试成功
		Details: &search.Details{Fetch: func(_ context.Context, ids []string) (map[string]string, error) {
			if flaky {
				flaky = false
				fmt.Println("  detail: 详情服务抖动，重试")
				return nil, errors.New("unavailable")
			}
			m := make(map[string]string, len(ids))
			for _, id := range ids {
				m[id] = "详情" + id
			}
			return m, nil
		}},
		Logger: &search.Logger{Log: func(uid, query string, hits int) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf("%s:%q:%d", uid, query, hits))
			mu.Unlock()
		}},
		// 每个 call 任务都经过 metrics（graph.WrapCalls）：库本身不记录执行过程，
		// 走了哪个分支、哪一步被跳过、哪一路失败，都从这里观测
		Metrics: &search.Metrics{Observe: func(path, op string, _ time.Duration, err error) {
			mu.Lock()
			defer mu.Unlock()
			ran[path] = err
			c := calls[op]
			c[0]++
			if err != nil {
				c[1]++
			}
			calls[op] = c
		}},
	})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()

	ask := func(uid, query string) {
		fmt.Printf("search(%s, %q)\n", uid, query)
		clear(ran)
		results, err := eng.Search(ctx, uid, query)
		if err != nil {
			fmt.Println("  error:", err)
			return
		}
		// 超时被放弃的向量召回可能在 Search 返回后才结束，等它跑完再看观测结果
		if err := eng.Drain(ctx); err != nil {
			panic(err)
		}
		mu.Lock()
		fmt.Println("  recall/vector:", orOK(ran["search/recall/vector"]))
		_, reranked := ran["search/rerank"]
		fmt.Println("  rerank ran:", reranked)
		_, personal := ran["search/rank/personal/p"]
		fmt.Println("  personalized:", personal)
		mu.Unlock()
		for _, r := range results {
			fmt.Printf("  %s %s (%s)\n", r.ID, r.Title, r.Detail)
		}
	}

	// 1. 有历史行为的用户：两路召回都在，合并后 4 条（够多）走精排，再按偏好（编译）个性化排序
	ask("alice", "go 调度")

	// 2. 新用户 + 慢查询：向量召回 50ms 超时被丢弃（optional），结果太少跳过精排，走默认排序子图
	ask("bob", "yaml slow")

	// 3. 热更新一份写错的拓扑：编译期一次报出全部问题，旧拓扑继续服务
	err = eng.Reload([]byte(`
graphs:
  search:
    do:
      - recall: { call: recall, with: { source: titel, limit: 20 } }
      - fill: { call: fill_detail }
`))
	var ce *graph.CompileError
	if errors.As(err, &ce) {
		fmt.Println("reload rejected:")
		for _, p := range ce.Problems {
			fmt.Println("  " + p.String())
		}
	}
	ask("bob", "cel")

	// 4. 停机前等被放弃的工作跑完（background 的搜索日志在每次 Search 返回前就已结束）
	if err := eng.Drain(ctx); err != nil {
		panic(err)
	}
	slices.Sort(logs)
	fmt.Println("logs:", logs)
	var stats []string
	for _, op := range slices.Sorted(maps.Keys(calls)) {
		stats = append(stats, fmt.Sprintf("%s=%d/%d", op, calls[op][0], calls[op][1]))
	}
	fmt.Println("calls/errors:", strings.Join(stats, " "))

	// Output:
	// search(alice, "go 调度")
	//   detail: 详情服务抖动，重试
	//   recall/vector: ok
	//   rerank ran: true
	//   personalized: true
	//   d1 go 并发调度 (详情d1)
	//   d5 go 编译原理 (详情d5)
	//   d2 go 内存模型 (详情d2)
	//   d3 调度器设计 (详情d3)
	// search(bob, "yaml slow")
	//   recall/vector: graph: search/recall/vector (recall): context deadline exceeded
	//   rerank ran: false
	//   personalized: false
	//   d4 yaml 配置入门 (详情d4)
	// reload rejected:
	//   5:9: search/recall: operator "recall" config: source must be title or vector, got "titel"
	//   5:9: search/recall: operator "recall" reads "parsed", but nothing before this point writes it
	// search(bob, "cel")
	//   recall/vector: ok
	//   rerank ran: false
	//   personalized: false
	//   d6 cel 表达式 (详情d6)
	// logs: [alice:"go 调度":4 bob:"cel":1 bob:"yaml slow":1]
	// calls/errors: fill_detail=4/1 load_profile=3/0 merge=3/0 parse_query=3/0 personalize=1/0 recall=6/1 rerank=1/0 search_log=3/0 sort_by_score=2/0
}

func orOK(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}

func testDeps() search.Deps {
	return search.Deps{
		Details: &search.Details{Fetch: func(_ context.Context, ids []string) (map[string]string, error) {
			return map[string]string{}, nil
		}},
		Logger:  &search.Logger{Log: func(string, string, int) {}},
		Metrics: &search.Metrics{Observe: func(string, string, time.Duration, error) {}},
	}
}

// 算子是普通函数，直接调用即可单测，不需要框架。
func TestMerge(t *testing.T) {
	out, err := search.Merge(context.Background(), search.MergeIn{Primary: []search.Doc{{ID: "d1", Score: 1}}})
	if err != nil || len(out.Docs) != 1 || out.Docs[0].Score != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	out, _ = search.Merge(context.Background(), search.MergeIn{
		Primary: []search.Doc{{ID: "d1", Score: 1}},
		Extra:   graph.Some([]search.Doc{{ID: "d1", Score: 2}, {ID: "d2", Score: 1}}),
	})
	if len(out.Docs) != 2 || out.Docs[0].Score != 3 || out.Docs[1].ID != "d2" {
		t.Fatalf("%+v", out)
	}
}

// 整图测试：用 Override 把 load_profile 换成替身（签名必须一致），验证新用户也能走个性化排序。
func TestGraphWithFakeProfile(t *testing.T) {
	opts, err := search.Options(testDeps())
	if err != nil {
		t.Fatal(err)
	}
	fake := graph.Override(graph.Op("load_profile", func(context.Context, search.ProfileIn) (search.ProfileOut, error) {
		return search.ProfileOut{Profile: &search.Profile{HasHistory: true, Likes: []string{"编译"}}}, nil
	}))
	p, err := graph.Compile(search.DefaultDoc(), append(opts, fake)...)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := graph.Run(context.Background(), p, search.Search, search.Request{UID: "bob", Query: "go"})
	if err != nil {
		t.Fatal(err)
	}
	// 默认排序是 d1 d2 d5；个性化后命中偏好（编译）的 d5 排到 d2 前面。
	var ids []string
	for _, r := range resp.Results {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"d1", "d5", "d2"}) {
		t.Errorf("results %v; want the personalized order d1 d5 d2", ids)
	}
}

// 中间件和算子一样直接调用即可单测：自己构造 Wrap，Next 用替身。
func TestTiming(t *testing.T) {
	var got []string
	m := &search.Metrics{Observe: func(path, op string, _ time.Duration, err error) {
		got = append(got, fmt.Sprint(path, " ", op, " ", err))
	}}
	failed := errors.New("boom")
	err := m.Timing(context.Background(), graph.Wrap[struct{}]{
		Path: "search/merge",
		Op:   "merge",
		Next: func(context.Context) error { return failed },
	})
	if !errors.Is(err, failed) || len(got) != 1 || got[0] != "search/merge merge boom" {
		t.Fatalf("err %v, observed %v", err, got)
	}
}

// Options 不接受缺依赖，问题在启动时暴露，而不是运行到那个算子才 panic。
func TestOptionsRejectMissingDeps(t *testing.T) {
	d := testDeps()
	d.Details = nil
	if _, err := search.Options(d); err == nil {
		t.Fatal("want an error")
	}
}

// 热更新换下的 Program 还有后台工作时，Engine.Drain 也要等它。
// 超时被放弃、但不看 ctx 的工作会比 Run 活得久：热更新换下的 Program 还有这种工作时，
// Engine.Drain 也要等它。（background 任务不算：Run 返回前它们已经结束。）
func TestDrainWaitsForReplacedPrograms(t *testing.T) {
	release := make(chan struct{})
	d := testDeps()
	d.Logger = &search.Logger{Log: func(string, string, int) { <-release }} // 不看 ctx，卡到 release
	eng, err := search.NewEngine(d)
	if err != nil {
		t.Fatal(err)
	}
	// 把日志放在一个带 timeout 的可丢任务里：超时后被放弃，goroutine 还卡着。
	err = eng.Reload([]byte(`
graphs:
  search:
    do:
      - parse:
          call: parse_query
      - recall:
          call: recall
          with:
            source: title
            limit: 5
      - fill:
          call: fill_detail
      - log:
          call: search_log
          timeout: 5ms
          optional: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Search(context.Background(), "alice", "go"); err != nil {
		t.Fatal(err)
	}
	// 卡住的日志属于被换下的 Program；再换一次，它仍忙，不能被当成空闲丢掉。
	for range 2 {
		if err := eng.Reload(search.DefaultDoc()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := eng.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain returned %v while the old Program's log is still running", err)
	}
	close(release)
	if err := eng.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
