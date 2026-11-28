package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/shadewright-labs/sablecourt/internal/attach"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store/storetest"
)

func (c *fixtureConn) SetWriteDeadline(time.Time) error { return nil }

// TestDockerInputOnStdinNeverInEnvironment launches every corpus input and
// checks that the container's create options (what docker inspect shows)
// carry neither the input nor its secret, and that the input arrives as one
// JSON line on stdin, closed afterwards when no Takeover hub is set (with a hub, the hub owns
// stdin and closes it when the log stream ends)
// (issue 261).
func TestDockerInputOnStdinNeverInEnvironment(t *testing.T) {
	ctx := context.Background()
	for _, c := range storetest.TaskInputCorpus() {
		for _, withHub := range []bool{false, true} {
			conn := &trackingConn{}
			eng := &fullAttachEngine{attachConn: conn, fakeEngine: fakeEngine{inspectState: &container.State{}}}
			a := testAdapter(eng, "")
			if withHub {
				a.AttachHub = attach.NewHub()
			}
			spec := scruntime.LaunchSpec{
				TaskID:    "t-input",
				InputJSON: c.Input,
				Workload:  domain.Workload{Spec: domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Image: "img:1"}}},
			}
			if _, err := a.Start(ctx, spec); err != nil {
				t.Fatalf("%s: Start: %v", c.Name, err)
			}
			opts, _ := json.Marshal(eng.createdOpts)
			if strings.Contains(string(opts), c.Secret.Body) || strings.Contains(string(opts), "SABLECOURT_INPUT_JSON") {
				t.Errorf("%s: create options carry the input: %s", c.Name, opts)
			}
			line := conn.body.String()
			if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
				t.Fatalf("%s: stdin frame %q is not one line", c.Name, line)
			}
			if !eng.createdOpts.Config.OpenStdin || eng.createdOpts.Config.StdinOnce == withHub || (!withHub && !conn.closed) {
				t.Errorf("%s hub=%t: OpenStdin=%t StdinOnce=%t closed=%t", c.Name, withHub, eng.createdOpts.Config.OpenStdin, eng.createdOpts.Config.StdinOnce, conn.closed)
			}
			if withHub {
				a.AttachHub.Close(spec.TaskID)
			}
		}
	}
}

// TestDockerInputNotDeliveredFailsClosed: when stdin cannot be attached or
// written, Start returns ErrInputNotDelivered and the input never goes to
// the environment instead.
func TestDockerInputNotDeliveredFailsClosed(t *testing.T) {
	ctx := context.Background()
	spec := scruntime.LaunchSpec{
		TaskID:    "t-undelivered",
		InputJSON: `{"prompt":"hello"}`,
		Workload:  domain.Workload{Spec: domain.WorkloadSpec{Runtime: domain.RuntimeConfig{Image: "img:1"}}},
	}
	cases := []struct {
		name    string
		eng     EngineClient
		started bool
	}{
		{name: "engine cannot attach", eng: &fakeEngine{}},
		{name: "attach fails", eng: &fullAttachEngine{attachErr: errors.New("attach refused")}},
		{name: "write fails", eng: &fullAttachEngine{attachConn: &trackingConn{fixtureConn: fixtureConn{fail: "write"}}}, started: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := testAdapter(tc.eng, "").Start(ctx, spec)
			if !errors.Is(err, scruntime.ErrInputNotDelivered) {
				t.Fatalf("Start = %v, want ErrInputNotDelivered", err)
			}
			if h.ExternalID == "" {
				t.Fatal("handle of the created container is lost, so it cannot be stopped")
			}
			var started string
			switch e := tc.eng.(type) {
			case *fakeEngine:
				started = e.startedID
			case *fullAttachEngine:
				started = e.startedID
			}
			if (started != "") != tc.started {
				t.Fatalf("started = %q, want started %t", started, tc.started)
			}
		})
	}
}

var _ io.Writer = (*fixtureConn)(nil)

// stdinAttachEngine lets the council fixture engine, which accepts only a
// full attach, serve the stdin-only attach Adapter.Start makes.
type stdinAttachEngine struct{ *councilEngine }

func (e stdinAttachEngine) ContainerAttach(ctx context.Context, id string, o client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	o.Stdout, o.Stderr = true, true
	return e.councilEngine.ContainerAttach(ctx, id, o)
}
