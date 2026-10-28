package search

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

// ---- 算子的输入输出 ----
//
// tag 是算子自己的端口名，只描述算子本身（docs、query、profile），不写图里的名字。
// 默认端口名就是黑板上的 key；图里要接到别的 key 时，在 search.yaml 的 in: / out: 里改名。

type ParseIn struct {
	Query string `graph:"query"`
}
type ParseOut struct {
	Parsed *Query `graph:"parsed"`
}

// RecallCfg 是 recall 的任务配置（YAML 的 with:）：同一个算子按配置召回不同的源。
type RecallCfg struct {
	Source string `yaml:"source"` // title 或 vector
	Limit  int    `yaml:"limit"`
}

// Validate 在编译期校验配置：热更新下发的拓扑写错了源，编译就会拒收。
func (c RecallCfg) Validate() error {
	if c.Source != "title" && c.Source != "vector" {
		return fmt.Errorf("source must be title or vector, got %q", c.Source)
	}
	if c.Limit <= 0 {
		return errors.New("limit must be > 0")
	}
	return nil
}

// RecallIn 是 recall 的输入：任务配置 + 解析后的查询。
type RecallIn struct {
	Cfg   RecallCfg `graph:",with"`
	Query *Query    `graph:"parsed"`
}

// DocsOut 是召回和各个排序算子的输出。recall 在图里被调用两次，用 out: 分别接到
// title_docs / vec_docs（fork 的两个分支不能写同一个 key）。
type DocsOut struct {
	Docs []Doc `graph:"docs"`
}

// ProfileIn 是 load_profile 的输入。导出 In / Out，测试才能用 graph.Override 换上签名一致的替身。
type ProfileIn struct {
	UID string `graph:"uid"`
}

// ProfileOut 是 load_profile 的输出。
type ProfileOut struct {
	Profile *Profile `graph:"profile"`
}

// MergeIn 是 merge 的输入：主召回 primary 和可选的补充召回 extra。
// 接哪一路由 search.yaml 的 in: 决定，算子不知道图里有 title / vector。
type MergeIn struct {
	Primary []Doc              `graph:"primary"`
	Extra   graph.Maybe[[]Doc] `graph:"extra"` // 接的是 optional 任务的输出，可能缺席，只能用 Maybe 读
}

type RerankIn struct {
	Docs  []Doc  `graph:"docs"`
	Query *Query `graph:"parsed"`
}

type PersonalizeIn struct {
	Docs    []Doc    `graph:"docs"`
	Profile *Profile `graph:"profile"`
}

type SortIn struct {
	Docs []Doc `graph:"docs"`
}

type FillIn struct {
	Docs []Doc `graph:"docs"`
}
type FillOut struct {
	Results []Result `graph:"results"`
}

type LogIn struct {
	UID   string `graph:"uid"`
	Query string `graph:"query"`
	Docs  []Doc  `graph:"docs"`
}

// ---- 纯算子：无依赖，导出后可直接单测 ----

// ParseQuery 读 query，写 parsed。
func ParseQuery(_ context.Context, in ParseIn) (ParseOut, error) {
	terms := strings.Fields(in.Query)
	if len(terms) == 0 {
		return ParseOut{}, errors.New("empty query")
	}
	return ParseOut{Parsed: &Query{Terms: terms}}, nil
}

var corpus = []Doc{
	{ID: "d1", Title: "go 并发调度"},
	{ID: "d2", Title: "go 内存模型"},
	{ID: "d3", Title: "调度器设计"},
	{ID: "d4", Title: "yaml 配置入门"},
	{ID: "d5", Title: "go 编译原理"},
	{ID: "d6", Title: "cel 表达式"},
}

// related 模拟向量召回的「语义相近」：查询词 → 相近的词。
var related = map[string][]string{
	"go":   {"并发"},
	"调度":   {"并发"},
	"yaml": {"配置"},
}

// Recall 按 with: 里的 source 召回：title 按标题命中的查询词计分，vector 按相近词计分。
// 查询带 "slow" 时向量召回要 200ms（模拟慢服务），并且遵守 ctx。
func Recall(ctx context.Context, in RecallIn) (DocsOut, error) {
	words := in.Query.Terms
	if in.Cfg.Source == "vector" {
		if slices.Contains(in.Query.Terms, "slow") {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return DocsOut{}, ctx.Err()
			}
		}
		words = nil
		for _, t := range in.Query.Terms {
			words = append(words, related[t]...)
		}
	}
	var docs []Doc
	for _, d := range corpus {
		for _, w := range words {
			if strings.Contains(d.Title, w) {
				d.Score++
			}
		}
		if d.Score > 0 {
			docs = append(docs, d)
		}
		if len(docs) == in.Cfg.Limit {
			break
		}
	}
	return DocsOut{Docs: docs}, nil
}

// LoadProfile 按 uid 加载用户画像；alice 有历史行为，偏好编译相关的内容。
func LoadProfile(_ context.Context, in ProfileIn) (ProfileOut, error) {
	if in.UID == "alice" {
		return ProfileOut{Profile: &Profile{HasHistory: true, Likes: []string{"编译"}}}, nil
	}
	return ProfileOut{Profile: &Profile{}}, nil
}

// Merge 合并主召回和补充召回：按 ID 去重，同一文档的分数相加，主召回的顺序在前。
func Merge(_ context.Context, in MergeIn) (DocsOut, error) {
	docs := slices.Clone(in.Primary)
	if !in.Extra.OK {
		return DocsOut{Docs: docs}, nil
	}
	for _, e := range in.Extra.V {
		if i := slices.IndexFunc(docs, func(d Doc) bool { return d.ID == e.ID }); i >= 0 {
			docs[i].Score += e.Score
		} else {
			docs = append(docs, e)
		}
	}
	return DocsOut{Docs: docs}, nil
}

// Rerank 精排：标题包含全部查询词的文档分数翻倍，再按分数排序。
func Rerank(_ context.Context, in RerankIn) (DocsOut, error) {
	docs := slices.Clone(in.Docs)
	for i, d := range docs {
		if !slices.ContainsFunc(in.Query.Terms, func(t string) bool { return !strings.Contains(d.Title, t) }) {
			docs[i].Score *= 2
		}
	}
	return DocsOut{Docs: byScore(docs)}, nil
}

// Personalize 个性化排序：命中用户偏好的文档加分，再按分数排序。
func Personalize(_ context.Context, in PersonalizeIn) (DocsOut, error) {
	docs := slices.Clone(in.Docs)
	for i, d := range docs {
		for _, like := range in.Profile.Likes {
			if strings.Contains(d.Title, like) {
				docs[i].Score++
			}
		}
	}
	return DocsOut{Docs: byScore(docs)}, nil
}

// SortByScore 通用排序：只按分数。
func SortByScore(_ context.Context, in SortIn) (DocsOut, error) {
	return DocsOut{Docs: byScore(slices.Clone(in.Docs))}, nil
}

// byScore 按分数从高到低排序，同分保持原顺序。
func byScore(docs []Doc) []Doc {
	slices.SortStableFunc(docs, func(a, b Doc) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	return docs
}

// ---- 有外部依赖的算子和中间件 ----
//
// 按依赖的角色各写一个结构体，算子是它们的方法，由 options.go 把实例绑定成选项。

// Details 是详情服务。
type Details struct {
	Fetch func(ctx context.Context, ids []string) (map[string]string, error)
}

// Fill 读 docs，向详情服务批量取详情，写 results。
func (s *Details) Fill(ctx context.Context, in FillIn) (FillOut, error) {
	ids := make([]string, len(in.Docs))
	for i, d := range in.Docs {
		ids[i] = d.ID
	}
	details, err := s.Fetch(ctx, ids)
	if err != nil {
		return FillOut{}, err
	}
	results := make([]Result, len(in.Docs))
	for i, d := range in.Docs {
		results[i] = Result{ID: d.ID, Title: d.Title, Detail: details[d.ID]}
	}
	return FillOut{Results: results}, nil
}

// Logger 记录搜索日志。
type Logger struct {
	Log func(uid, query string, hits int)
}

// SearchLog 记录一次搜索，在 search.yaml 里以 background 执行：不阻塞后面的任务。
func (l *Logger) SearchLog(_ context.Context, in LogIn) (struct{}, error) {
	l.Log(in.UID, in.Query, len(in.Docs))
	return struct{}{}, nil
}

// Metrics 收集每个算子的耗时和结果。
type Metrics struct {
	Observe func(path, op string, cost time.Duration, err error)
}

// Timing 是观测中间件。options.go 用 graph.WrapCalls 把它套在每个 call 任务上，
// 不用改 search.yaml；w.Path 是任务路径，w.Op 是算子名。
func (m *Metrics) Timing(ctx context.Context, w graph.Wrap[struct{}]) error {
	start := time.Now()
	err := w.Next(ctx)
	m.Observe(w.Path, w.Op, time.Since(start), err)
	return err
}
