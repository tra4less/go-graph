package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	graph "github.com/tra4less/go-graph/graph"
)

func TestSwitchAndSubgraph(t *testing.T) {
	cases := []struct {
		in         MainIn
		want, last string // last: the call that produced the answer
	}{
		{in: MainIn{UID: "vip", Query: "a b"}, want: "vip:a,b", last: "main/route/vip/gen"},
		{in: MainIn{UID: "bob", Query: "a b"}, want: "plain:bob", last: "main/route/default/gen/answer"},
		{in: MainIn{UID: "bob", Query: "a b", Hint: graph.Some("h")}, want: "plain:bob:h", last: "main/route/default/gen/answer"},
	}
	for _, c := range cases {
		calls, rec := recordCalls()
		p := mustCompile(t, optsOf(baseOptions()...), answerDoc, rec...)
		out, err := graph.Run(context.Background(), p, Main, c.in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Answer != c.want {
			t.Errorf("%+v: answer %q, want %q", c.in, out.Answer, c.want)
		}
		if !calls.ran(c.last) {
			t.Errorf("%+v: %s did not run; ran %v", c.in, c.last, calls.paths())
		}
	}
}

// A switch without a default fails when no case matches, so a key written by
// every case is guaranteed after it.
func TestSwitchNoCase(t *testing.T) {
	p := mustCompile(t, optsOf(baseOptions()...), `
graphs:
  main:
    do:
      - user: { call: load_user }
      - parse: { call: parse }
      - route:
          switch:
            - vip:
                when: "user.Level >= 3"
                do: [ { gen: { call: vip_answer } } ]
            - plain:
                when: "user.Level < 2"
                do: [ { gen: { call: plain_answer } } ]
`)
	out, err := graph.Run(context.Background(), p, Main, MainIn{UID: "vip", Query: "q"})
	if err != nil || out.Answer != "vip:q" {
		t.Fatalf("vip: %q, %v", out.Answer, err)
	}

	// Levels are 1 or 5, so add a case no user can match to get no match.
	p = mustCompile(t, optsOf(baseOptions()...), `
graphs:
  main:
    do:
      - user: { call: load_user }
      - parse: { call: parse }
      - route:
          switch:
            - mid:
                when: "user.Level == 3"
                do: [ { gen: { call: vip_answer } } ]
`)
	_, err = graph.Run(context.Background(), p, Main, MainIn{UID: "bob", Query: "q"})
	var te *graph.TaskError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "no switch case matched") || te.Path != "main/route" {
		t.Fatalf("err %v", err)
	}

	// An empty default skips the switch; the key then may be absent.
	_, err = graph.Compile([]byte(`
graphs:
  main:
    do:
      - user: { call: load_user }
      - parse: { call: parse }
      - route:
          switch:
            - mid: { when: "user.Level == 3", do: [ { gen: { call: vip_answer } } ] }
            - default: { do: [] }
`), baseOptions()...)
	if err == nil || !strings.Contains(err.Error(), `Entry output "answer" may be absent`) {
		t.Errorf("empty default: %v", err)
	}
}
