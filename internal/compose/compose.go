// Package compose lets Shearwater's operation protocol run one service of a
// Docker Compose project. A model still gets only a scoped delegation, an
// exact plan hash, operator approval, a receipt and per-replica HTTP proof;
// this package only knows how to read and change the Compose service.
package compose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/operation"
	"shearwater/internal/store"
)

const (
	generationLabel = "shearwater.io/generation"
	operationLabel  = "shearwater.io/operation"
	projectLabel    = "com.docker.compose.project"
	serviceLabel    = "com.docker.compose.service"
)

// Runner runs the docker compose CLI with arguments after the compose command
// itself and returns its combined output.
type Runner func(ctx context.Context, args ...string) (string, error)

// Backend operates services of one Compose project. Changes are made by
// `docker compose up` with an override file that sets only the service image
// and Shearwater's labels; the user's compose files are never rewritten.
type Backend struct {
	Project  string
	Files    []string // absolute compose files, in -f order
	StateDir string   // private directory for override files
	Docker   Docker
	Run      Runner
	// Timeout bounds one `docker compose up`, including an image pull.
	Timeout time.Duration

	mu        sync.Mutex // serializes Execute; Observe never waits for it
	cacheMu   sync.Mutex
	cacheKey  string
	cacheConf map[string]json.RawMessage
}

// CLI returns a Runner for the given compose command, such as
// ["docker", "compose"], with extra environment such as DOCKER_HOST.
func CLI(command []string, dir string, env []string) Runner {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, command[0], append(slices.Clone(command[1:]), args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		text := out.String()
		if len(text) > 16<<10 {
			text = text[len(text)-16<<10:]
		}
		if err != nil {
			return text, fmt.Errorf("docker compose %s: %w: %s", args[len(args)-1], err, strings.TrimSpace(text))
		}
		return text, nil
	}
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func fail(code, message string) error { return &operation.Error{Code: code, Message: message} }

// UID is the stable identity of one Compose service. Compose has no object
// UID; the generation label detects replacement by hand.
func (b *Backend) UID(service string) string { return "compose/" + b.Project + "/" + service }

func (b *Backend) fileArgs() []string {
	args := []string{"--project-name", b.Project}
	for _, f := range b.Files {
		args = append(args, "--file", f)
	}
	return args
}

// services returns the normalized compose model from `docker compose config`,
// cached until a compose file changes.
func (b *Backend) services(ctx context.Context) (map[string]json.RawMessage, error) {
	var key strings.Builder
	for _, f := range b.Files {
		st, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&key, "%s:%d:%d;", f, st.Size(), st.ModTime().UnixNano())
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if key.String() == b.cacheKey && b.cacheConf != nil {
		return b.cacheConf, nil
	}
	out, err := b.Run(ctx, append(b.fileArgs(), "config", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	var model struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err = json.Unmarshal([]byte(out), &model); err != nil {
		return nil, fmt.Errorf("read compose config: %w", err)
	}
	b.cacheKey, b.cacheConf = key.String(), model.Services
	return model.Services, nil
}

type port struct {
	Target    int32      `json:"target"`
	Published flexString `json:"published"`
	HostIP    string     `json:"host_ip"`
	Protocol  string     `json:"protocol"`
}

// flexString accepts the published port as a JSON string or number; compose
// versions have printed both.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

type service struct {
	Image  string `json:"image"`
	Ports  []port `json:"ports"`
	Scale  *int32 `json:"scale"`
	Deploy *struct {
		Replicas *int32 `json:"replicas"`
	} `json:"deploy"`
	// base is a digest of every user setting except the image, which plans
	// change through the override file.
	base string
}

func (b *Backend) service(ctx context.Context, name string) (service, error) {
	var s service
	all, err := b.services(ctx)
	if err != nil {
		return s, err
	}
	raw, ok := all[name]
	if !ok {
		return s, fail("INVALID", "the compose files define no service "+strconv.Quote(name))
	}
	if err = json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		return s, err
	}
	delete(fields, "image")
	s.base = digest(fields)
	return s, nil
}

// Check reads the compose model once so a wrong file, project or compose
// command fails when the server starts rather than on the first request.
func (b *Backend) Check(ctx context.Context) ([]string, error) {
	all, err := b.services(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func (s service) replicas() int32 {
	switch {
	case s.Deploy != nil && s.Deploy.Replicas != nil:
		return *s.Deploy.Replicas
	case s.Scale != nil:
		return *s.Scale
	}
	return 1
}

// route binds the container port the checks use and, when that port is
// published on the host, the host address that fronts every replica.
func (b *Backend) route(s service, name string, want int32) (*operation.Route, error) {
	var tcp []port
	for _, p := range s.Ports {
		if (p.Protocol == "" || p.Protocol == "tcp") && (want == 0 || p.Target == want) {
			tcp = append(tcp, p)
		}
	}
	if want == 0 && len(tcp) != 1 {
		return nil, fail("AMBIGUOUS", "select the container port to check explicitly")
	}
	target := want
	route := &operation.Route{Name: name, Selector: map[string]string{projectLabel: b.Project, serviceLabel: name}}
	if len(tcp) > 0 {
		p := tcp[0]
		target = p.Target
		if p.Published != "" {
			published, err := strconv.Atoi(string(p.Published))
			if err != nil || published < 1 || published > 65535 {
				return nil, fail("UNSUPPORTED", "select a single published host port, not a range")
			}
			host := p.HostIP
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !ip.Is4() {
				return nil, fail("UNSUPPORTED", "the published port must use an IPv4 host address")
			}
			route.IP = ip.String()
			route.Port.Port = int32(published)
		}
	}
	if target < 1 || target > 65535 {
		return nil, fail("INVALID", "select a container TCP port")
	}
	route.Port.Protocol = "TCP"
	route.Port.TargetPort = apis.IntOrString{Int: target}
	return route, nil
}

// state is the managed identity found on the service's containers. Mixed is
// true when containers carry different releases, which only an interrupted
// rollout of the same plan may resume; everything else treats it as stale.
type state struct {
	containers []Container
	running    []Container
	generation int64
	marker     string
	mixed      bool
}

func (b *Backend) state(ctx context.Context, name string) (state, error) {
	var s state
	all, err := b.Docker.Containers(ctx, b.Project, name)
	if err != nil {
		return s, err
	}
	slices.SortFunc(all, func(a, b Container) int { return strings.Compare(a.ID, b.ID) })
	s.containers = all
	for i, c := range all {
		if c.State.Running || c.State.Restarting {
			s.running = append(s.running, c)
		}
		gen, _ := strconv.ParseInt(c.Config.Labels[generationLabel], 10, 64)
		marker := c.Config.Labels[operation.Marker]
		if i > 0 && (gen != s.generation || marker != s.marker) {
			s.mixed = true
		}
		if i == 0 {
			s.generation, s.marker = gen, marker
		}
	}
	return s, nil
}

func (s state) carries(hash string) bool {
	for _, c := range s.containers {
		if c.Config.Labels[operation.Marker] == hash {
			return true
		}
	}
	return false
}

func (s state) ids() []string {
	ids := make([]string, 0, len(s.containers))
	for _, c := range s.containers {
		ids = append(ids, c.ID)
	}
	return ids
}

var errMixed = fail("STALE", "service containers belong to different releases; finish or repair the rollout by hand first")

func (b *Backend) Managed(addr netip.Addr) bool {
	return addr.Is4() && (addr.IsLoopback() || addr.IsPrivate())
}

func (b *Backend) project(r operation.Record) error {
	if r.Spec.Namespace != b.Project {
		return fail("INVALID", "this server operates compose project "+strconv.Quote(b.Project)+"; use it as the namespace")
	}
	return nil
}

func (b *Backend) Bind(_ *store.Tx, r *operation.Record) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.project(*r); err != nil {
		return false, err
	}
	s, err := b.service(ctx, r.Spec.Name)
	if err != nil {
		return false, err
	}
	if n := s.replicas(); n < 1 || n > 16 {
		return false, fail("UNSUPPORTED", "operations support 1..16 replicas")
	}
	st, err := b.state(ctx, r.Spec.Name)
	if err != nil || len(st.containers) == 0 {
		return false, err
	}
	if st.mixed {
		return false, errMixed
	}
	route, err := b.route(s, r.Spec.Name, r.Spec.Port)
	if err != nil {
		return false, err
	}
	r.UID, r.Generation, r.Route = b.UID(r.Spec.Name), st.generation, route
	if r.Spec.Image == "" {
		r.Spec.Image = st.containers[0].Config.Image
	}
	return true, nil
}

func (b *Backend) Reserve(_ *store.Tx, r operation.Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if r.Spec.Service != "" && r.Spec.Service != r.Spec.Name {
		return fail("INVALID", "compose apps are selected by service name only")
	}
	s, err := b.service(ctx, r.Spec.Name)
	if err != nil {
		return err
	}
	_, err = b.route(s, r.Spec.Name, r.Spec.Port)
	return err
}

func (b *Backend) Vacant(_ *store.Tx, r operation.Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := b.state(ctx, r.Spec.Name)
	if err != nil {
		return err
	}
	if len(st.containers) > 0 {
		return fail("STALE", "the service already has containers; reconnect to operate it")
	}
	return nil
}

// base checks that the bound service still matches the record and returns the
// digest a plan is based on.
func (b *Backend) base(ctx context.Context, r operation.Record) (service, state, string, error) {
	s, err := b.service(ctx, r.Spec.Name)
	if err != nil {
		return s, state{}, "", err
	}
	st, err := b.state(ctx, r.Spec.Name)
	if err != nil {
		return s, st, "", err
	}
	if st.mixed {
		return s, st, "", errMixed
	}
	marker, own := expectedMarker(r)
	if len(st.containers) == 0 || st.generation != r.Generation || own && st.marker != marker {
		return s, st, "", fail("STALE", "app was changed outside this operation; operator must reconnect")
	}
	route, err := b.route(s, r.Spec.Name, r.Spec.Port)
	if err != nil {
		return s, st, "", err
	}
	if digest(route) != digest(r.Route) {
		return s, st, "", fail("STALE", "the published port or container port changed")
	}
	return s, st, digest([]any{s.base, st.generation, st.marker}), nil
}

func (b *Backend) Base(_ *store.Tx, r operation.Record) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, st, spec, err := b.base(ctx, r)
	if err != nil {
		return "", "", err
	}
	return spec, st.containers[0].Config.Image, nil
}

func (b *Backend) Apply(*store.Tx, *operation.Record, operation.Plan) error {
	return errors.New("the compose backend executes plans outside the ledger")
}

func (b *Backend) override(r operation.Record, p operation.Plan, generation int64) (string, error) {
	if err := os.MkdirAll(b.StateDir, 0o700); err != nil {
		return "", err
	}
	doc := map[string]any{"services": map[string]any{r.Spec.Name: map[string]any{
		"image": p.Image,
		"labels": map[string]string{
			operation.Marker: p.Hash,
			generationLabel:  strconv.FormatInt(generation, 10),
			operationLabel:   r.ID,
		},
	}}}
	data, _ := json.MarshalIndent(doc, "", "  ")
	path := b.overridePath(r)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// Execute applies a plan with `docker compose up`. The plan hash label makes it
// idempotent: when the service already carries the plan it only reports it.
// Without --wait, readiness stays the job of verify, as with the engine.
func (b *Backend) Execute(ctx context.Context, r operation.Record, p operation.Plan) (operation.Target, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.project(r); err != nil {
		return operation.Target{}, operation.Unchanged(err)
	}
	generation := p.BaseGeneration + 1
	if p.Action == "deploy" {
		generation = 1
	}
	s, err := b.service(ctx, r.Spec.Name)
	if err != nil {
		return operation.Target{}, operation.Unchanged(err)
	}
	route, err := b.route(s, r.Spec.Name, r.Spec.Port)
	if err != nil {
		return operation.Target{}, operation.Unchanged(err)
	}
	target := operation.Target{UID: b.UID(r.Spec.Name), Generation: generation, Route: route}
	before, err := b.state(ctx, r.Spec.Name)
	if err != nil {
		return operation.Target{}, err
	}
	// The override file is written just before `up`, so it records that an
	// earlier attempt of this plan may have removed containers already.
	attempted := b.attempted(r, p.Hash)
	carries := before.carries(p.Hash)
	if carries && before.mixed && !attempted {
		// Another operation replaced the override and started its rollout.
		// A delayed retry must not recreate the earlier release over it.
		return operation.Target{}, errMixed
	}
	if !carries && (!attempted || len(before.containers) != 0) {
		// Nothing of this plan exists yet: it may only start from its base.
		if p.Action == "deploy" {
			if len(before.containers) > 0 {
				return operation.Target{}, operation.Unchanged(fail("STALE", "service containers appeared after approval"))
			}
		} else if _, _, spec, err := b.base(ctx, r); err != nil {
			return operation.Target{}, operation.Unchanged(err)
		} else if spec != p.BaseSpec {
			return operation.Target{}, operation.Unchanged(fail("STALE", "app specification changed after plan approval"))
		}
	} else if carries && !before.mixed && len(before.containers) == int(s.replicas()) {
		// Already applied, for example before a lost response: report only.
		return target, nil
	}
	file, err := b.override(r, p, generation)
	if err != nil {
		return operation.Target{}, operation.Unchanged(err)
	}
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	run, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(b.fileArgs(), "--file", file, "up", "--detach", "--no-deps", "--no-build", "--pull", "missing", r.Spec.Name)
	if _, err = b.Run(run, args...); err != nil {
		// Only call the app unchanged when the same containers remain and none
		// carries this plan; a removed or new container means a partial change.
		after, readErr := b.state(ctx, r.Spec.Name)
		if attempted || readErr != nil || after.carries(p.Hash) || !slices.Equal(after.ids(), before.ids()) {
			return operation.Target{}, err
		}
		return operation.Target{}, operation.Unchanged(err)
	}
	return target, nil
}

func (b *Backend) overridePath(r operation.Record) string {
	return filepath.Join(b.StateDir, b.Project+"-"+r.Spec.Name+".override.json")
}

// attempted reports whether the service's override file was last written for
// this plan, meaning `docker compose up` already ran for it at least once.
func (b *Backend) attempted(r operation.Record, hash string) bool {
	data, err := os.ReadFile(b.overridePath(r))
	if err != nil {
		return false
	}
	var doc struct {
		Services map[string]struct {
			Labels map[string]string `json:"labels"`
		} `json:"services"`
	}
	return json.Unmarshal(data, &doc) == nil && doc.Services[r.Spec.Name].Labels[operation.Marker] == hash
}

// expectedMarker is the plan hash the containers of the record's generation
// carry, or "" when this operation has not applied that generation itself.
func expectedMarker(r operation.Record) (string, bool) {
	for i := len(r.Plans) - 1; i >= 0; i-- {
		if p := r.Plans[i]; p.Applied && p.Generation == r.Generation {
			return p.Hash, true
		}
	}
	return "", false
}

func (b *Backend) Observe(_ *store.Tx, r operation.Record) (operation.Observation, error) {
	var o operation.Observation
	if !b.mu.TryLock() {
		o.Facts = append(o.Facts, "a compose change is running; verify again shortly")
		return o, nil
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := b.service(ctx, r.Spec.Name)
	if err != nil {
		return o, err
	}
	st, err := b.state(ctx, r.Spec.Name)
	if err != nil {
		return o, err
	}
	marker, own := expectedMarker(r)
	if st.mixed || st.generation != r.Generation || own && st.marker != marker {
		o.External = true
		return o, nil
	}
	route, err := b.route(s, r.Spec.Name, r.Spec.Port)
	if err != nil {
		return o, err
	}
	if digest(route) != digest(r.Route) {
		return o, fail("STALE", "the published port or container port changed")
	}
	want := s.replicas()
	image := ""
	if len(st.containers) > 0 {
		image = st.containers[0].Config.Image
	}
	var ready []operation.Replica
	var identities []any
	for _, c := range st.containers {
		if c.State.Status == "dead" {
			o.Failed = true
			o.Facts = []string{"Docker reports a dead container for this release"}
			return o, nil
		}
		healthy := c.State.Running && !c.State.Restarting && (c.State.Health == nil || c.State.Health.Status == "healthy")
		if !healthy || c.Config.Image != image {
			continue
		}
		ip := ""
		for _, n := range c.NetworkSettings.Networks {
			if addr, err := netip.ParseAddr(n.IPAddress); err == nil && b.Managed(addr) {
				ip = addr.String()
				break
			}
		}
		if ip == "" {
			continue
		}
		imageID, err := b.imageID(ctx, c)
		if err != nil {
			return o, err
		}
		rep := operation.Replica{UID: c.ID, ContainerID: c.ID, ImageID: imageID, IP: ip, Port: r.Route.Port.TargetPort.Int}
		ready = append(ready, rep)
		identities = append(identities, []any{rep.UID, rep.IP, rep.ImageID, rep.Port})
	}
	o.Facts = append(o.Facts, fmt.Sprintf("goal generation=%d; ready containers=%d/%d; containers present=%d", r.Generation, len(ready), want, len(st.containers)))
	if int32(len(ready)) != want || len(st.containers) != len(ready) {
		return o, nil
	}
	slices.SortFunc(ready, func(a, b operation.Replica) int { return strings.Compare(a.UID, b.UID) })
	o.Ready, o.Image, o.Replicas = true, image, ready
	o.FrontIP, o.FrontPort = r.Route.IP, r.Route.Port.Port
	o.Scope = "every new container directly"
	if o.FrontIP != "" {
		o.Scope += " and the published host port"
	}
	o.Fingerprint = digest([]any{r.UID, r.Generation, s.base, image, r.Route, r.GoalChecks, identities})
	return o, nil
}

// imageID returns an immutable repository digest for the container's image
// when one exists, so verified evidence can name a durable recovery image.
func (b *Backend) imageID(ctx context.Context, c Container) (string, error) {
	if repo, digest, ok := strings.Cut(c.Config.Image, "@sha256:"); ok {
		return repo + "@sha256:" + digest, nil
	}
	info, err := b.Docker.Image(ctx, c.Image)
	if err != nil {
		return "", err
	}
	repo := c.Config.Image
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, d := range info.RepoDigests {
		if name, _, _ := strings.Cut(d, "@"); name == repo || strings.HasSuffix(name, "/"+repo) || strings.HasSuffix(repo, "/"+name) {
			return repo + "@" + strings.SplitN(d, "@", 2)[1], nil
		}
	}
	return c.Image, nil
}

func (b *Backend) Diagnose(_ *store.Tx, r operation.Record, result *operation.Result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := b.Docker.Containers(ctx, b.Project, r.Spec.Name)
	if err != nil {
		return err
	}
	result.Facts = append(result.Facts, fmt.Sprintf("compose service %s/%s: containers=%d; intended generation=%d", b.Project, r.Spec.Name, len(st), r.Generation))
	for i, c := range st {
		if i >= 8 {
			break
		}
		health := "none"
		if c.State.Health != nil {
			health = c.State.Health.Status
		}
		reason := c.State.Status
		if c.State.Error != "" {
			reason += ": " + c.State.Error
		}
		if len(reason) > 120 {
			reason = reason[:120]
		}
		gen := c.Config.Labels[generationLabel]
		result.Facts = append(result.Facts, fmt.Sprintf("container %s: image=%s generation=%s health=%s exit=%d restarts=%d state=%q", strings.TrimPrefix(c.Name, "/"), c.Config.Image, gen, health, c.State.ExitCode, c.RestartCount, reason))
	}
	return nil
}

var _ operation.Backend = (*Backend)(nil)
var _ operation.Executor = (*Backend)(nil)
