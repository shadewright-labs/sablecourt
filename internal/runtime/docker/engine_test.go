package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type missing struct{ error }

func (missing) NotFound() {}

type inspectResponseTransport struct {
	status int
	body   string
}

func (tr inspectResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Request:    req,
		StatusCode: tr.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(tr.body)),
	}, nil
}

func TestIsMissingInspectResponse(t *testing.T) {
	const absent = `{"message":"No such container: fixture"}`
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		missing bool
	}{
		{"present", http.StatusOK, `{"Id":"fixture"}`, false},
		{"removed", http.StatusNotFound, absent, true},
		{"forbidden", http.StatusForbidden, absent, false},
		{"daemon failure", http.StatusInternalServerError, absent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := client.New(client.WithAPIVersion(client.MaxAPIVersion),
				client.WithHTTPClient(&http.Client{Transport: inspectResponseTransport{tc.status, tc.body}}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cli.Close() })
			_, err = cli.ContainerInspect(t.Context(), "fixture", client.ContainerInspectOptions{})
			if (err == nil) != (tc.status == http.StatusOK) || isMissing(err) != tc.missing {
				t.Fatalf("inspect HTTP %d returned %v, missing=%t, want missing=%t", tc.status, err, isMissing(err), tc.missing)
			}
		})
	}
}

type inspectEngine struct {
	fakeEngine
	net              network.Network
	imageID          string
	netErr, imageErr error
	pulled           bool
}

func (f *inspectEngine) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{Network: network.Inspect{Network: f.net}}, f.netErr
}
func (f *inspectEngine) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	r := client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: f.imageID}}
	if err := json.Unmarshal([]byte(`{}`), &r.Config); err != nil {
		return r, err
	}
	return r, f.imageErr
}
func (f *inspectEngine) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.pulled = true
	return nil, nil
}

func TestStartResolvesLocalResources(t *testing.T) {
	for _, tc := range []string{"pinned", "external bridge", "wrong driver", "network unavailable", "image missing", "image unavailable", "invalid digest"} {
		t.Run(tc, func(t *testing.T) {
			f := &inspectEngine{net: network.Network{Name: DefaultNetworkName, ID: "net-id", Driver: "bridge", Internal: true}, imageID: "sha256:" + strings.Repeat("a", 64)}
			switch tc {
			case "external bridge":
				f.net.Internal = false
			case "wrong driver":
				f.net.Driver = "host"
			case "network unavailable":
				f.netErr = errors.New("unavailable")
			case "image missing":
				f.imageErr = missing{errors.New("missing")}
			case "image unavailable":
				f.imageErr = errors.New("unavailable")
			case "invalid digest":
				f.imageID = ""
			}
			spec := rt.LaunchSpec{TaskID: "task"}
			spec.Workload.Spec.Runtime.Image = "fixture"
			_, err := NewAdapter(f, "").Start(t.Context(), spec)
			if tc == "pinned" {
				if err != nil || f.createdOpts.Config.Image != f.imageID || string(f.createdOpts.HostConfig.NetworkMode) != f.net.ID {
					t.Fatal("did not create by inspected IDs", err)
				}
			} else if err == nil || f.createdOpts.Config != nil || f.createdNetwork.Driver != "" || f.pulled {
				t.Fatal("launched, pulled or created resources on failed inspect")
			} else if isMissing(err) != (tc == "image missing") {
				t.Fatalf("error %v has missing=%t, want %t", err, isMissing(err), tc == "image missing")
			}
		})
	}
}

type recoveryEngine struct {
	fakeEngine
	state *container.State
	id    string
	err   error
}

func (f recoveryEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: f.id, State: f.state}}, f.err
}

func TestDockerRecoveryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		state                            *container.State
		wantAlive, wantExited, wantError bool
	}{
		{"missing", nil, false, false, false}, {"transient", nil, false, false, true}, {"missing state", nil, false, false, true},
		{"wrong identity", &container.State{Status: "running", Running: true}, false, false, true},
		{"running", &container.State{Status: "running", Running: true}, true, false, false},
		{"paused", &container.State{Status: "paused", Running: true, Paused: true}, true, false, false},
		{"created", &container.State{Status: "created"}, false, false, true},
		{"unfinished", &container.State{Status: "exited"}, false, false, true},
		{"exited", &container.State{Status: "exited", FinishedAt: "2026-01-02T03:04:05Z"}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &recoveryEngine{state: tc.state, id: strings.Repeat("a", 64)}
			h := rt.Handle{ExternalID: f.id, AdapterName: "docker"}
			switch tc.name {
			case "missing":
				f.err = missing{errors.New("gone")}
			case "transient":
				f.err = errors.New("unavailable")
			case "wrong identity":
				f.id = "other"
			}
			a := NewAdapter(f, "")
			probes, ok := any(a).(interface {
				rt.HandleLivenessProber
				rt.HandleExitProber
			})
			if !ok {
				t.Fatal("Docker recovery lacks evidence probes")
			}
			alive, err := probes.HandleAlive(t.Context(), h)
			if alive != tc.wantAlive || (err != nil) != tc.wantError {
				t.Fatalf("alive=%v error=%v", alive, err)
			}
			exit, err := probes.HandleExited(t.Context(), h)
			if exit != tc.wantExited || (err != nil) != tc.wantError {
				t.Fatalf("exited=%v error=%v", exit, err)
			}
		})
	}
}
