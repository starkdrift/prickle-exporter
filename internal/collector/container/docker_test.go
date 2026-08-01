// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// fakeDockerSocket serves the captured GET /containers/json response over a
// unix socket and returns its path.
//
// The response body is testdata/.../docker-api/containers.json, captured from
// the same host as the cgroup tree, so the container IDs in it are the IDs of
// the Docker scopes the walk finds.
func fakeDockerSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	// A unix socket path is capped near 100 bytes; t.TempDir() with a long test
	// name can exceed that, so the socket goes in a short directory of its own.
	dir, err := os.MkdirTemp("", "prickle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	return path
}

// serveCapturedContainers is the happy path: the captured response, for the
// path and method SPEC.md §Hard constraints #2 permits and no other.
func serveCapturedContainers(t *testing.T) http.HandlerFunc {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, "docker-api", "containers.json"))
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Docker request used %s; only GET is permitted", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/containers/json" {
			t.Errorf("Docker request for %s, want /containers/json", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

// TestDockerEnrichment checks that names and images reach prickle_container_info
// and nothing else.
func TestDockerEnrichment(t *testing.T) {
	socket := fakeDockerSocket(t, serveCapturedContainers(t))
	out := collectFixture(t, func(o *Options) { o.DockerSocket = socket })

	// The three Docker containers in the capture, with the names the API gave.
	for _, want := range []string{
		`container="48c2b913843a172adff5fb81b2e0a0d1e4916f03c3b0c47d6b746875465c9d74",pod="",pod_name="",runtime="docker",qos="",name="fixture-nginx",image="nginx:alpine"`,
		`container="8d09d3c34a61c442f29773b0d7f9278273cc9ad0c6bba7338469ff531bd1e48b",pod="",pod_name="",runtime="docker",qos="",name="fixture-sleeper",image="busybox"`,
		`container="f705060e0a39b2f27fb84e358e140c56e348b7db8a203950078282c298e44dd2",pod="",pod_name="",runtime="docker",qos="",name="fixture-redis",image="redis:alpine"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prickle_container_info is missing %s", want)
		}
	}

	// SPEC.md §Metrics contract: descriptive attributes never reach a hot
	// series. The name must appear on the _info gauge and nowhere else.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "prickle_container_info") {
			continue
		}
		if strings.Contains(line, "fixture-nginx") || strings.Contains(line, "nginx:alpine") {
			t.Errorf("a hot series carries a descriptive attribute: %s", line)
		}
	}

	// The 13 containerd containers are not Docker's, and get no name from it.
	if n := countMatching(out, "prickle_container_info{"); n != fixtureContainers {
		t.Errorf("info series = %d, want %d", n, fixtureContainers)
	}
	// The leading comma matters: `pod_name=""` contains `name=""` as a
	// substring, so the bare form counts both and silently doubled when
	// pod_name was added.
	if n := strings.Count(out, `,name=""`); n != fixtureKubernetes {
		t.Errorf("unnamed info series = %d, want %d (the containerd containers)", n, fixtureKubernetes)
	}
}

// TestDockerIsOffByDefault checks that no socket is opened unless one is
// configured — the exporter does not go looking for a daemon to talk to.
func TestDockerIsOffByDefault(t *testing.T) {
	out := collectFixture(t)
	if !strings.Contains(out, `name="",image=""`) {
		t.Error("info gauge carries names without an enrichment socket configured")
	}

	meta, err := New(Options{Roots: fsroot.At(fixtureDir)}).dockerNames(context.Background())
	if err != nil || meta != nil {
		t.Errorf("dockerNames with no socket = %v, %v; want nil, nil", meta, err)
	}
}

// TestDockerFailureCostsOnlyTheNames is the contract from SPEC.md §Collectors:
// the socket is an enrichment path. A daemon that is down, wedged or answering
// nonsense must not cost the metrics of the containers it started.
func TestDockerFailureCostsOnlyTheNames(t *testing.T) {
	tests := []struct {
		name   string
		socket func(t *testing.T) string
	}{{
		name:   "no socket at that path",
		socket: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.sock") },
	}, {
		name: "daemon returns an error status",
		socket: func(t *testing.T) string {
			return fakeDockerSocket(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "server error", http.StatusInternalServerError)
			})
		},
	}, {
		name: "daemon returns something that is not JSON",
		socket: func(t *testing.T) string {
			return fakeDockerSocket(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("<html>not the API you were looking for"))
			})
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(Options{Roots: fsroot.At(fixtureDir), DockerSocket: tt.socket(t)})

			set := newFixtureSet()
			err := c.Collect(context.Background(), set)
			if err == nil {
				t.Error("expected the failure to be reported, so prickle_collector_errors_total rises")
			}
			out := set.String()
			if n := countMatching(out, "prickle_container_memory_usage_bytes{"); n != fixtureContainers {
				t.Errorf("memory_usage_bytes series = %d, want %d: a Docker failure cost container metrics", n, fixtureContainers)
			}
			if n := countMatching(out, "prickle_container_info{"); n != fixtureContainers {
				t.Errorf("info series = %d, want %d", n, fixtureContainers)
			}
		})
	}
}

// TestDockerTimeoutIsBounded checks that a daemon which accepts the connection
// and then never answers costs DockerTimeout and not the scrape.
func TestDockerTimeoutIsBounded(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	socket := fakeDockerSocket(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	})

	c := New(Options{
		Roots:         fsroot.At(fixtureDir),
		DockerSocket:  socket,
		DockerTimeout: 50 * time.Millisecond,
	})

	set := newFixtureSet()
	start := time.Now()
	err := c.Collect(context.Background(), set)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected a timeout error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("collection took %s; the Docker timeout did not bound it", elapsed)
	}
	if n := countMatching(set.String(), "prickle_container_info{"); n != fixtureContainers {
		t.Errorf("info series = %d, want %d", n, fixtureContainers)
	}
}
