package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Docker reads containers and images from the Docker Engine API. Only the few
// read calls the backend needs are implemented; every change goes through the
// docker compose CLI so the user's compose files stay the source of truth.
type Docker interface {
	Containers(ctx context.Context, project, service string) ([]Container, error)
	Image(ctx context.Context, ref string) (ImageInfo, error)
}

// Container is the part of `GET /containers/{id}/json` the backend uses.
type Container struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"` // local image ID, sha256:...
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"` // reference as compose requested it
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ImageInfo is the part of `GET /images/{name}/json` the backend uses.
type ImageInfo struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

// Engine talks HTTP to dockerd over its Unix socket.
type Engine struct {
	client *http.Client
}

func NewEngine(socket string) *Engine {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	return &Engine{client: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
}

func (e *Engine) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("docker engine: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Message string `json:"message"`
		}
		json.Unmarshal(body, &msg)
		return &engineError{Status: resp.StatusCode, Message: msg.Message}
	}
	return json.Unmarshal(body, v)
}

type engineError struct {
	Status  int
	Message string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("docker engine HTTP %d: %s", e.Status, e.Message)
}

// Containers lists every container, stopped ones included, that compose
// created for this project's service, then reads each one in full.
func (e *Engine) Containers(ctx context.Context, project, service string) ([]Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {
		"com.docker.compose.project=" + project,
		"com.docker.compose.service=" + service,
	}})
	var list []struct {
		ID string `json:"Id"`
	}
	if err := e.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &list); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		var full Container
		err := e.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", &full)
		if ee, ok := err.(*engineError); ok && ee.Status == http.StatusNotFound {
			continue // removed between list and inspect
		}
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

func (e *Engine) Image(ctx context.Context, ref string) (ImageInfo, error) {
	var info ImageInfo
	err := e.get(ctx, "/images/"+url.PathEscape(ref)+"/json", &info)
	return info, err
}
