package supervisor

import (
	"flag"
	"testing"
)

func TestSandboxSelection(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "container"},
		{[]string{"--sandbox", "example"}, "example"},
		{[]string{"--runner", "example"}, "example"},
	} {
		var opts Options
		fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
		opts.BindFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if opts.Backend != tc.want {
			t.Errorf("args %v selected %q, want %q", tc.args, opts.Backend, tc.want)
		}
	}
}
