package graph_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graph "github.com/tra4less/go-graph/graph"
)

type User struct {
	Name  string
	Level int
}

type parsed struct{ Words []string }

type userIn struct {
	UID string `graph:"uid"`
}

type userOut struct {
	User *User `graph:"user"`
}

type parseIn struct {
	Query string `graph:"query"`
}

type parseOut struct {
	Parsed *parsed `graph:"parsed"`
}

type answerIn struct {
	User   *User               `graph:"user"`
	Parsed *parsed             `graph:"parsed"`
	Hint   graph.Maybe[string] `graph:"hint"`
}

type answerOut struct {
	Answer string `graph:"answer"`
}

// MainIn and MainOut are the entry types of answerDoc's "main" graph.
type MainIn struct {
	UID   string              `graph:"uid"`
	Query string              `graph:"query"`
	Hint  graph.Maybe[string] `graph:"hint"`
}

type MainOut struct {
	Answer string `graph:"answer"`
}

var Main = graph.NewEntry[MainIn, MainOut]("main")

func baseOptions() []graph.Option { return append(baseOps(), Main) }

func optsOf(o ...graph.Option) []graph.Option { return o }

// baseOps are the operators of answerDoc, without an entry.
func baseOps() []graph.Option {
	return []graph.Option{
		graph.Op("load_user", func(_ context.Context, in userIn) (userOut, error) {
			level := 1
			if in.UID == "vip" {
				level = 5
			}
			return userOut{User: &User{Name: in.UID, Level: level}}, nil
		}),
		graph.Op("parse", func(_ context.Context, in parseIn) (parseOut, error) {
			return parseOut{Parsed: &parsed{Words: strings.Fields(in.Query)}}, nil
		}),
		graph.Op("vip_answer", func(_ context.Context, in answerIn) (answerOut, error) {
			return answerOut{Answer: "vip:" + strings.Join(in.Parsed.Words, ",")}, nil
		}),
		graph.Op("plain_answer", func(_ context.Context, in answerIn) (answerOut, error) {
			s := "plain:" + in.User.Name
			if in.Hint.OK {
				s += ":" + in.Hint.V
			}
			return answerOut{Answer: s}, nil
		}),
	}
}

// mustCompile compiles src with opts followed by extra.
func mustCompile(t *testing.T, opts []graph.Option, src string, extra ...graph.Option) *graph.Program {
	t.Helper()
	p, err := graph.Compile([]byte(src), append(slices.Clip(opts), extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const answerDoc = `
graphs:
  main:
    do:
      - user: { call: load_user }
      - parse: { call: parse }
      - route:
          switch:
            - vip:
                when: "user.Level >= 3 && uid != ''"
                do: [ { gen: { call: vip_answer } } ]
            - default:
                do: [ { gen: { run: plain } } ]
  plain:
    do: [ { answer: { call: plain_answer } } ]
`

// captureLog makes slog.Default record into the returned log for the rest
// of the test: background failures are logged there.
func captureLog(t *testing.T) *logRecords {
	t.Helper()
	l := &logRecords{}
	old := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

type logRecord struct {
	ctx   context.Context
	attrs map[string]string
}

type logRecords struct {
	mu   sync.Mutex
	recs []logRecord
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *logRecords) WithGroup(string) slog.Handler { return l }

func (l *logRecords) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, logRecord{ctx, attrs})
	l.mu.Unlock()
	return nil
}

func (l *logRecords) records() []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs)
}

// calls records, through WrapCalls, every call task of a run that reached
// its operator and the operator's error. It is how the tests see which
// branch ran or which task was skipped: the library itself records nothing.
type calls struct {
	mu sync.Mutex
	m  map[string]error
}

// recordCalls returns a recorder and the options that install it.
func recordCalls() (*calls, []graph.Option) {
	c := &calls{m: make(map[string]error)}
	return c, []graph.Option{
		graph.Middleware("record_calls", func(ctx context.Context, w graph.Wrap[struct{}]) error {
			err := w.Next(ctx)
			c.mu.Lock()
			c.m[w.Path] = err
			c.mu.Unlock()
			return err
		}),
		graph.WrapCalls("record_calls"),
	}
}

func (c *calls) ran(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[path]
	return ok
}

func (c *calls) err(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[path]
}

func (c *calls) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.m))
}

// g wraps task lines into a document with one graph "main", whose entry
// provides uid and query and maybe hint (MainIn).
func g(tasks ...string) string {
	var b strings.Builder
	b.WriteString("graphs:\n  main:\n    do:\n")
	for _, t := range tasks {
		b.WriteString("      " + t + "\n")
	}
	return b.String()
}

// Entry types shared by the behaviour tests.
type (
	NoIn struct{}
	AIn  struct {
		A string `graph:"a"`
	}
	// ABOut reads back what "put" may write.
	ABOut struct {
		A graph.Maybe[string] `graph:"a"`
		B graph.Maybe[string] `graph:"b"`
	}
	JoinedOut struct {
		Joined string `graph:"joined"`
	}
)

type wcfg struct {
	Key   string        `yaml:"key"`
	Val   string        `yaml:"val"`
	Sleep time.Duration `yaml:"sleep"`
	Fail  bool          `yaml:"fail"`
	Panic bool          `yaml:"panic"`
	Deaf  bool          `yaml:"deaf"` // ignores ctx while sleeping
}

type sharedOut struct {
	Shared string `graph:"shared"`
}

type joinIn struct {
	A      string              `graph:"a"`
	B      graph.Maybe[string] `graph:"b"`
	Shared string              `graph:"shared"`
}

type joinOut struct {
	Joined string `graph:"joined"`
}

// behaviour builds a catalog of configurable test operators: "put" writes
// its configured value under key a, b or shared after an optional sleep.
func behaviour(t *testing.T, extra ...graph.Option) []graph.Option {
	t.Helper()
	act := func(ctx context.Context, c wcfg) error {
		if c.Panic {
			panic("boom")
		}
		if c.Sleep > 0 {
			if c.Deaf {
				time.Sleep(c.Sleep)
			} else {
				select {
				case <-time.After(c.Sleep):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if c.Fail {
			return errors.New("failed " + c.Val)
		}
		return nil
	}
	type abOut struct {
		A graph.Maybe[string] `graph:"a"`
		B graph.Maybe[string] `graph:"b"`
	}
	type cfgIn struct {
		Cfg wcfg `graph:",with"`
	}
	comps := append([]graph.Option{
		graph.Op("put", func(ctx context.Context, in cfgIn) (abOut, error) {
			c := in.Cfg
			if err := act(ctx, c); err != nil {
				return abOut{}, err
			}
			var o abOut
			switch c.Key {
			case "a":
				o.A = graph.Some(c.Val)
			case "b":
				o.B = graph.Some(c.Val)
			}
			return o, nil
		}),
		graph.Op("put_shared", func(ctx context.Context, in cfgIn) (sharedOut, error) {
			return sharedOut{Shared: in.Cfg.Val}, act(ctx, in.Cfg)
		}),
		graph.Op("join", func(_ context.Context, in joinIn) (joinOut, error) {
			b := "-"
			if in.B.OK {
				b = in.B.V
			}
			return joinOut{Joined: in.A + "|" + b + "|" + in.Shared}, nil
		}),
	}, extra...)
	return comps
}

func deafOp(running *atomic.Int32, d time.Duration) graph.Option {
	return graph.Op("deaf", func(context.Context, struct{}) (struct{}, error) {
		running.Add(1)
		defer running.Add(-1)
		time.Sleep(d)
		return struct{}{}, nil
	})
}
