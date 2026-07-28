// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// dockerMeta is what the Docker API adds to a container the cgroup walk has
// already found: the two human-readable attributes, and nothing else.
//
// SPEC.md §Collectors calls this an enrichment path "for human-readable names
// only". Both values land on prickle_container_info and never on a hot series.
type dockerMeta struct {
	name  string
	image string
}

// dockerContainer is the subset of GET /containers/json this package decodes.
//
// Declaring the three fields rather than decoding into a map is what keeps the
// enrichment honest: the response also carries network addresses, mounts, port
// bindings and image digests, none of which belong in a metrics label.
type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
}

// dockerNames queries the Docker API for the running containers' names.
//
// SPEC.md §Hard constraints #2 permits GET-only requests on the Docker socket
// and nothing more, which is the whole of what this does. It is off unless
// Options.DockerSocket is set.
//
// A failure returns the error and an empty map: the collector still reports
// every container it found in the cgroup tree, without names. A daemon that is
// down must not cost the metrics of the containers it started.
func (c *Collector) dockerNames(ctx context.Context) (map[string]dockerMeta, error) {
	if c.opts.DockerSocket == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.DockerTimeout)
	defer cancel()

	// The host in the URL is ignored — the transport dials the socket path —
	// but net/http requires a syntactically valid one.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json", nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.opts.DockerSocket)
		},
	}}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker socket %s: %w", c.opts.DockerSocket, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker socket %s: %s", c.opts.DockerSocket, resp.Status)
	}

	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("docker socket %s: %w", c.opts.DockerSocket, err)
	}

	meta := make(map[string]dockerMeta, len(containers))
	for _, dc := range containers {
		meta[dc.ID] = dockerMeta{name: dockerName(dc.Names), image: dc.Image}
	}
	return meta, nil
}

// dockerName picks the container's name.
//
// The API returns names as a list of paths — ["/fixture-nginx"] — because a
// container could once be linked into another's namespace under an alias. The
// first entry is the container's own name; the leading slash is a path
// separator, not part of it.
func dockerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
