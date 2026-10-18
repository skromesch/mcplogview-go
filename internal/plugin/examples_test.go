package plugin

import (
	"bytes"
	"context"
	"testing"
)

// The example plugins ship with Lua tests; keep them green.
func TestExamplePluginsPassTheirTests(t *testing.T) {
	for _, dir := range []string{"../../examples/plugins/hello"} {
		var out bytes.Buffer
		res, err := RunTests(context.Background(), dir, TestOptions{Out: &out})
		if err != nil || res.Failed != 0 || res.Passed == 0 {
			t.Errorf("%s: res=%+v err=%v\n%s", dir, res, err, out.String())
		}
	}
}
