package logs

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

func dockerSrc() config.Source {
	return config.Source{Type: config.SourceDocker, ID: "d", Label: "D", Container: "my-api"}
}

func withLocal(t *testing.T, fn func(script string) ([]byte, error)) {
	t.Helper()
	old := localRun
	localRun = func(_ context.Context, script string) ([]byte, error) { return fn(script) }
	t.Cleanup(func() { localRun = old })
}

const (
	t1 = "2026-10-17T10:00:00.000000001Z"
	t2 = "2026-10-17T10:00:00.000000002Z"
	t3 = "2026-10-17T10:00:01.500000000Z"
)

func TestReadDockerPagingWithSharedTimestamps(t *testing.T) {
	body := strings.Join([]string{t1 + " a", t1 + " b", t2 + " c", t3 + " d"}, "\n") + "\n"
	var lastScript string
	withLocal(t, func(script string) ([]byte, error) {
		lastScript = script
		return []byte("__MCPLOGVIEW_DOCKER_ID__:id1\n" + body), nil
	})

	r1, err := ReadDocker(dockerSrc(), "", 1)
	if err != nil || len(r1.Lines) != 1 || r1.Lines[0] != t1+" a" || r1.EOF {
		t.Fatalf("page1: %+v %v", r1, err)
	}
	r2, _ := ReadDocker(dockerSrc(), r1.Cursor, 2)
	if got := strings.Join(r2.Lines, "|"); got != t1+" b|"+t2+" c" || r2.EOF {
		t.Fatalf("page2 must continue inside the shared timestamp: %q", got)
	}
	if !strings.Contains(lastScript, "SINCE='2026-10-17T09:59:59.75") {
		t.Fatalf("cursor should become --since with a small overlap: %s", lastScript)
	}
	r3, _ := ReadDocker(dockerSrc(), r2.Cursor, 10)
	if got := strings.Join(r3.Lines, "|"); got != t3+" d" || !r3.EOF {
		t.Fatalf("page3: %q eof=%v", got, r3.EOF)
	}
	r4, _ := ReadDocker(dockerSrc(), r3.Cursor, 10)
	if len(r4.Lines) != 0 || !r4.EOF || r4.Lines == nil {
		t.Fatalf("nothing new: %+v", r4)
	}
}

func TestReadDockerCursorFromOtherContainerOrIncarnationIsIgnored(t *testing.T) {
	withLocal(t, func(script string) ([]byte, error) {
		id := "id1"
		if strings.Contains(script, "EXPECT='id1'") {
			id = "id2" // container was recreated
		}
		return []byte("__MCPLOGVIEW_DOCKER_ID__:" + id + "\n" + t1 + " a\n" + t2 + " b\n"), nil
	})
	r1, _ := ReadDocker(dockerSrc(), "", 10)
	r2, _ := ReadDocker(dockerSrc(), r1.Cursor, 10)
	if len(r2.Lines) != 2 {
		t.Fatalf("recreated container must restart from the beginning: %+v", r2)
	}

	other := dockerSrc()
	other.Container = "other"
	r3, _ := ReadDocker(other, r1.Cursor, 10)
	if len(r3.Lines) != 2 {
		t.Fatalf("foreign cursor must be ignored: %+v", r3)
	}
}

func TestReadDockerErrors(t *testing.T) {
	cases := []struct {
		name, out string
		check     func(error) bool
	}{
		{"missing", "__MCPLOGVIEW_ERR__:inspect:Error: No such container: my-api\n", func(e error) bool {
			var nf *ContainerNotFoundError
			return errors.As(e, &nf)
		}},
		{"no docker", "__MCPLOGVIEW_ERR__:nodocker\n", func(e error) bool {
			var de *DockerError
			return errors.As(e, &de) && strings.Contains(de.Msg, "docker CLI not found")
		}},
		{"daemon down", "__MCPLOGVIEW_ERR__:inspect:Cannot connect to the Docker daemon\n", func(e error) bool {
			var de *DockerError
			return errors.As(e, &de) && strings.Contains(de.Msg, "Cannot connect")
		}},
		{"garbage", "hello\n", func(e error) bool {
			var iv *InvalidSSHResponseError
			return errors.As(e, &iv)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var runErr error
			if strings.Contains(c.out, "__MCPLOGVIEW_ERR__") {
				runErr = errors.New("exit status 12")
			}
			withLocal(t, func(string) ([]byte, error) { return []byte(c.out), runErr })
			if _, err := ReadDocker(dockerSrc(), "", 1); err == nil || !c.check(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestDockerRemoteGoesThroughSSH(t *testing.T) {
	src := dockerSrc()
	src.Host, src.User = "prod", "ubuntu"
	var args []string
	withSSH(t, func(a []string) ([]byte, error) {
		args = a
		return []byte("__MCPLOGVIEW_DOCKER_ID__:x\n" + t1 + " hi\n"), nil
	})
	r, err := ReadChunk(src, "", 10)
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ubuntu@prod") || strings.Contains(joined, "-p ") || strings.Contains(joined, "-i ") {
		t.Fatalf("unset port/identity must be left to ssh config: %s", joined)
	}
	if !strings.Contains(joined, "docker container inspect") {
		t.Fatalf("%s", joined)
	}
}

func TestDockerScriptQuotesUntrustedValues(t *testing.T) {
	s := dockerScript("a'b", "", "", false)
	if !strings.Contains(s, `'a'\''b'`) {
		t.Fatalf("container name must be shell-quoted: %s", s)
	}
}

func TestListDirRemoteParsing(t *testing.T) {
	withSSH(t, func([]string) ([]byte, error) { return []byte("syslog\nnginx/\nauth.log\napt/\n"), nil })
	got, err := ListDir(config.Source{Host: "h"}, "/var/log")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, fmt.Sprintf("%s:%v", e.Name, e.IsDir))
	}
	if strings.Join(names, ",") != "apt:true,nginx:true,auth.log:false,syslog:false" {
		t.Fatalf("%v", names)
	}
}

// ---- real Docker -----------------------------------------------------------

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", "alpine:latest").Run(); err != nil {
		t.Skip("alpine:latest not present locally")
	}
}

func startTicker(t *testing.T, name string) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name, "alpine:latest", "sh", "-c",
		`i=0; while true; do echo "tick $i"; echo "err $i" >&2; i=$((i+1)); sleep 0.1; done`).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

func TestRealDockerCursorNoLossNoDuplicates(t *testing.T) {
	requireDocker(t)
	name := "mcplogview-it-" + strconv.Itoa(time.Now().Nanosecond())
	startTicker(t, name)
	src := config.Source{Type: config.SourceDocker, ID: "it", Label: "IT", Container: name}

	if err := Probe(src); err != nil {
		t.Fatalf("probe: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	var all []string
	cursor := ""
	for round := 0; round < 3; round++ {
		for i := 0; i < 50; i++ { // drain in small pages
			r, err := ReadChunk(src, cursor, 4)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, r.Lines...)
			cursor = r.Cursor
			if r.EOF {
				break
			}
		}
		time.Sleep(700 * time.Millisecond) // more output arrives between rounds
	}

	seen := map[string]int{}
	for _, l := range all {
		seen[l]++
	}
	for l, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate line across pages/rounds: %q x%d", l, n)
		}
	}
	// stdout and stderr both present, and ticks are gapless 0..n-1
	maxTick := -1
	for _, l := range all {
		if f := strings.Fields(l); len(f) == 3 && f[1] == "tick" {
			n, _ := strconv.Atoi(f[2])
			if n > maxTick {
				maxTick = n
			}
		}
	}
	if maxTick < 15 {
		t.Fatalf("expected a few seconds of ticks, got max %d (%d lines)", maxTick, len(all))
	}
	for i := 0; i <= maxTick; i++ {
		if seen[findLine(all, "tick "+strconv.Itoa(i))] == 0 || seen[findLine(all, "err "+strconv.Itoa(i))] == 0 {
			t.Fatalf("missing tick/err %d in output", i)
		}
	}
}

func findLine(lines []string, suffix string) string {
	for _, l := range lines {
		if strings.HasSuffix(l, " "+suffix) {
			return l
		}
	}
	return ""
}

func TestRealDockerMissingContainerAndList(t *testing.T) {
	requireDocker(t)
	src := config.Source{Type: config.SourceDocker, ID: "x", Label: "X", Container: "mcplogview-does-not-exist"}
	var nf *ContainerNotFoundError
	if _, err := ReadChunk(src, "", 1); !errors.As(err, &nf) {
		t.Fatalf("got %v", err)
	}
	if err := Probe(src); !errors.As(err, &nf) {
		t.Fatalf("probe: %v", err)
	}

	name := "mcplogview-it-list-" + strconv.Itoa(time.Now().Nanosecond())
	startTicker(t, name)
	cs, err := ListContainers(config.Source{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cs {
		if c.Name == name && strings.HasPrefix(c.Image, "alpine") {
			found = true
		}
	}
	if !found {
		t.Fatalf("test container not listed among %d containers", len(cs))
	}
}

func TestRealDockerRecreatedContainerResetsCursor(t *testing.T) {
	requireDocker(t)
	name := "mcplogview-it-re-" + strconv.Itoa(time.Now().Nanosecond())
	startTicker(t, name)
	src := config.Source{Type: config.SourceDocker, ID: "it", Label: "IT", Container: name}
	time.Sleep(600 * time.Millisecond)

	r1, err := ReadChunk(src, "", 1000)
	if err != nil || len(r1.Lines) == 0 {
		t.Fatalf("%+v %v", r1, err)
	}
	startTicker(t, name) // removes and recreates under the same name
	time.Sleep(600 * time.Millisecond)

	r2, err := ReadChunk(src, r1.Cursor, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// stdout/stderr order within the first ~100ms is not guaranteed
	if len(r2.Lines) < 2 || !strings.HasSuffix(r2.Lines[0], " tick 0") && !strings.HasSuffix(r2.Lines[0], " err 0") {
		t.Fatalf("new container must be read from its start, got first line %q", firstOr(r2.Lines))
	}
}

func firstOr(l []string) string {
	if len(l) == 0 {
		return "<none>"
	}
	return l[0]
}

func TestReadDockerDeliversLateLinesWithOlderTimestamp(t *testing.T) {
	// The daemon can append a line whose timestamp is older than one already
	// delivered. It must still reach the agent.
	const early = "2026-10-17T10:00:00.100000000Z"
	const late = "2026-10-17T10:00:00.100100000Z"
	const inverted = "2026-10-17T10:00:00.100050000Z" // older than `late`, written after it

	calls := 0
	withLocal(t, func(string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("__MCPLOGVIEW_DOCKER_ID__:x\n" + early + " e\n" + late + " l\n"), nil
		}
		return []byte("__MCPLOGVIEW_DOCKER_ID__:x\n" + early + " e\n" + late + " l\n" + inverted + " i\n"), nil
	})
	r1, _ := ReadDocker(dockerSrc(), "", 10)
	if len(r1.Lines) != 2 {
		t.Fatalf("%+v", r1)
	}
	r2, _ := ReadDocker(dockerSrc(), r1.Cursor, 10)
	if len(r2.Lines) != 1 || r2.Lines[0] != inverted+" i" {
		t.Fatalf("late line lost or duplicates delivered: %+v", r2)
	}
	r3, _ := ReadDocker(dockerSrc(), r2.Cursor, 10)
	if len(r3.Lines) != 0 {
		t.Fatalf("must not repeat: %+v", r3)
	}
}

func TestReadDockerCursorStaysSmall(t *testing.T) {
	var b strings.Builder
	b.WriteString("__MCPLOGVIEW_DOCKER_ID__:x\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "2026-10-17T10:00:00.%09dZ line %d\n", i, i)
	}
	withLocal(t, func(string) ([]byte, error) { return []byte(b.String()), nil })
	r, _ := ReadDocker(dockerSrc(), "", 2000)
	if len(r.Lines) != 2000 || len(r.Cursor) > 1200 {
		t.Fatalf("lines=%d cursor=%d bytes", len(r.Lines), len(r.Cursor))
	}
}
