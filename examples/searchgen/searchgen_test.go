package searchgen_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tra4less/go-graph/examples/search"
	"github.com/tra4less/go-graph/examples/searchgen"
	graph "github.com/tra4less/go-graph/graph"
)

// 生成的选项和手写的 search.Options 等价：同一份 search.yaml 能编译、能跑出相同结果。
func TestGeneratedOptionsRunSearch(t *testing.T) {
	opts, err := searchgen.Options(searchgen.Receivers{
		Details: &search.Details{Fetch: func(context.Context, []string) (map[string]string, error) { return nil, nil }},
		Logger:  &search.Logger{Log: func(string, string, int) {}},
		Metrics: &search.Metrics{Observe: func(string, string, time.Duration, error) {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := graph.Compile(search.DefaultDoc(), append(opts, graph.WrapCalls("metrics"))...)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := graph.Run(context.Background(), p, search.Search, search.Request{UID: "alice", Query: "go 调度"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range resp.Results {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"d1", "d5", "d2", "d3"}) {
		t.Errorf("results %v", ids)
	}
}

// 生成的 Options 不接受 nil 接收者，并一次列出所有缺失的接收者：问题在启动时暴露，而不是运行到那个算子才 panic。
func TestOptionsRejectNilReceiver(t *testing.T) {
	_, err := searchgen.Options(searchgen.Receivers{Metrics: &search.Metrics{}})
	want := "searchgen: Receivers.Details is nil (needed by fill_detail)\n" +
		"searchgen: Receivers.Logger is nil (needed by search_log)"
	if err == nil || err.Error() != want {
		t.Fatalf("err %v", err)
	}
}
