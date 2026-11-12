package api

// Table output (design 3.2): kubectl asks for
// "application/json;as=Table;g=meta.k8s.io;v=v1" and the server sends the
// columns and cells the Kubernetes printers would show. These tests read the
// same response kubectl reads, including the includeObject variants and the
// cells that come from status subresources.

import (
	"strings"
	"testing"
	"time"
)

const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io"

func tableOf(t *testing.T, r reply) map[string]any {
	t.Helper()
	if r.at("kind") != "Table" {
		t.Fatalf("want a Table, got: %s", r.raw)
	}
	return r.body
}

func columnNames(tb map[string]any) []string {
	cols, _ := tb["columnDefinitions"].([]any)
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		m, _ := c.(map[string]any)
		name, _ := m["name"].(string)
		out = append(out, name)
	}
	return out
}

func firstRowMap(t *testing.T, tb map[string]any) map[string]any {
	t.Helper()
	rows, _ := tb["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d: %v", len(rows), rows)
	}
	row, _ := rows[0].(map[string]any)
	return row
}

func firstRow(t *testing.T, tb map[string]any) []any {
	t.Helper()
	cells, _ := firstRowMap(t, tb)["cells"].([]any)
	return cells
}

func tableCells(t *testing.T, c *client, path string) []any {
	t.Helper()
	return firstRow(t, tableOf(t, c.want(c.do("GET", path, "", "Accept", tableAccept), 200)))
}

// TestTableColumns: every kind answers with its own columns, and a kind with no
// rows still sends them.
func TestTableColumns(t *testing.T) {
	c := newClient(t)
	for _, tc := range []struct{ path, columns string }{
		{pods, "Name,Ready,Status,Restarts,Age,IP,Node,Nominated Node,Readiness Gates"},
		{deploys, "Name,Ready,Up-to-date,Available,Age,Containers,Images,Selector"},
		{"/apis/apps/v1/namespaces/default/replicasets", "Name,Desired,Current,Ready,Age,Containers,Images,Selector"},
		{services, "Name,Type,Cluster-IP,External-IP,Port(s),Age,Selector"},
		{"/api/v1/nodes", "Name,Status,Roles,Age,Version,Internal-IP,External-IP,OS-Image,Kernel-Version,Container-Runtime"},
		{"/api/v1/namespaces", "Name,Status,Age"},
		{"/api/v1/namespaces/default/events", "Last Seen,Type,Reason,Object,Message,First Seen,Count,Name"},
		{"/apis/discovery.k8s.io/v1/namespaces/default/endpointslices", "Name,AddressType,Ports,Endpoints,Age"},
	} {
		tb := tableOf(t, c.want(c.do("GET", tc.path, "", "Accept", tableAccept), 200))
		if got := strings.Join(columnNames(tb), ","); got != tc.columns {
			t.Errorf("%s: columns are %q, want %q", tc.path, got, tc.columns)
		}
		if tb["apiVersion"] != "meta.k8s.io/v1" {
			t.Errorf("%s: apiVersion is %v", tc.path, tb["apiVersion"])
		}
	}
}

// TestTablePodCells: READY, STATUS and RESTARTS follow the container statuses.
func TestTablePodCells(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	if cells := tableCells(t, c, pods); cells[0] != "p1" || cells[1] != "0/1" || cells[2] != "Pending" || cells[3] != "0" {
		t.Fatalf("a new pod shows %v, want p1 0/1 Pending 0", cells)
	}

	// A running and ready container is the only thing "ready" means in stage 1:
	// there are no probes yet.
	put := func(status string) {
		t.Helper()
		c.want(c.do("PUT", pods+"/p1/status", `{"metadata":{"name":"p1"},"status":`+status+`}`), 200)
	}
	put(`{"phase":"Running","containerStatuses":[{"name":"c","ready":true,"restartCount":2,"image":"nginx:1.27","state":{"running":{}}}]}`)
	if cells := tableCells(t, c, pods); cells[1] != "1/1" || cells[2] != "Running" || cells[3] != "2" {
		t.Fatalf("a running pod shows %v, want 1/1 Running 2", cells)
	}

	put(`{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"image":"nginx:1.27","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}`)
	if cells := tableCells(t, c, pods); cells[1] != "0/1" || cells[2] != "ImagePullBackOff" {
		t.Fatalf("a waiting container shows %v, want 0/1 ImagePullBackOff", cells)
	}

	put(`{"phase":"Succeeded","containerStatuses":[{"name":"c","ready":false,"image":"nginx:1.27","state":{"terminated":{"exitCode":0,"reason":"Completed"}}}]}`)
	if cells := tableCells(t, c, pods); cells[2] != "Completed" {
		t.Fatalf("a terminated container shows %v, want the reason Completed", cells)
	}

	put(`{"phase":"Failed","containerStatuses":[{"name":"c","ready":false,"image":"nginx:1.27","state":{"terminated":{"exitCode":137}}}]}`)
	if cells := tableCells(t, c, pods); cells[2] != "ExitCode:137" {
		t.Fatalf("a killed container shows %v, want ExitCode:137", cells)
	}
}

// TestTableDeploymentReady: READY is ready replicas out of the desired number.
func TestTableDeploymentReady(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", deploys, twoDeploy), 201)
	if cells := tableCells(t, c, deploys); cells[0] != "web" || cells[1] != "0/2" {
		t.Fatalf("the deployment shows %v, want web 0/2", cells)
	}
}

// TestTableServicePorts checks the assigned ClusterIP and port/protocol list.
func TestTableServicePorts(t *testing.T) {
	c := newClient(t)
	// Two ports must be named; that is a validation rule, not a Table one.
	c.want(c.do("POST", services, `{"metadata":{"name":"web"},"spec":{"selector":{"app":"web"},"ports":[{"name":"http","port":80},{"name":"https","port":443}]}}`), 201)
	cells := tableCells(t, c, services)
	if cells[0] != "web" || cells[1] != "ClusterIP" || cells[2] != firstServiceIP || cells[3] != "<none>" || cells[4] != "80/TCP,443/TCP" {
		t.Fatalf("the service shows %v, want web ClusterIP %s <none> 80/TCP,443/TCP", cells, firstServiceIP)
	}
}

// TestTableNamespaceCells: the two namespaces every cluster starts with.
func TestTableNamespaceCells(t *testing.T) {
	c := newClient(t)
	tb := tableOf(t, c.want(c.do("GET", "/api/v1/namespaces", "", "Accept", tableAccept), 200))
	rows, _ := tb["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("want default and kube-system, got %d rows", len(rows))
	}
	for i, want := range []string{"default", "kube-system"} {
		row, _ := rows[i].(map[string]any)
		cells, _ := row["cells"].([]any)
		if cells[0] != want || cells[1] != "Active" {
			t.Fatalf("row %d is %v, want %s Active", i, cells, want)
		}
	}
}

// TestTableIncludeObject: what each includeObject value puts in the row.
func TestTableIncludeObject(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	for _, tc := range []struct{ mode, kind string }{
		{"", "PartialObjectMetadata"},
		{"Metadata", "PartialObjectMetadata"},
		{"Object", "Pod"},
		{"None", ""},
	} {
		path := pods
		if tc.mode != "" {
			path += "?includeObject=" + tc.mode
		}
		row := firstRowMap(t, tableOf(t, c.want(c.do("GET", path, "", "Accept", tableAccept), 200)))
		obj, hasObject := row["object"].(map[string]any)
		if tc.kind == "" {
			if hasObject {
				t.Errorf("includeObject=None left the object in the row: %v", obj)
			}
			continue
		}
		if !hasObject || obj["kind"] != tc.kind {
			t.Errorf("includeObject=%q put %v in the row, want a %s", tc.mode, row["object"], tc.kind)
		}
		if tc.kind == "PartialObjectMetadata" {
			md, _ := obj["metadata"].(map[string]any)
			if md["name"] != "p1" {
				t.Errorf("PartialObjectMetadata lost the metadata: %v", md)
			}
		}
	}
}

// TestTableGetOne: a single object can be asked for as a Table too.
func TestTableGetOne(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	tb := tableOf(t, c.want(c.do("GET", pods+"/p1", "", "Accept", tableAccept), 200))
	if rv, _ := tb["metadata"].(map[string]any)["resourceVersion"].(string); rv == "" {
		t.Fatalf("a Table for one object must carry its resourceVersion: %v", tb["metadata"])
	}
	if cells := firstRow(t, tb); cells[0] != "p1" {
		t.Fatalf("the row is %v, want p1", cells)
	}
}

// TestTableEmptyList: no rows, but the columns are still there.
func TestTableEmptyList(t *testing.T) {
	c := newClient(t)
	tb := tableOf(t, c.want(c.do("GET", pods, "", "Accept", tableAccept), 200))
	if rows, ok := tb["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("an empty list must have \"rows\": [], got %v", tb["rows"])
	}
	if len(columnNames(tb)) != len(tableColumns["Pod"]) {
		t.Fatalf("the empty list lost the columns: %v", columnNames(tb))
	}
}

// TestTableRefusesOtherAs: the server produces Table or plain JSON, and says so
// rather than guessing.
func TestTableRefusesOtherAs(t *testing.T) {
	c := newClient(t)
	r := c.want(c.do("GET", pods, "", "Accept", "application/json;as=PartialObjectMetadata;v=v1;g=meta.k8s.io"), 406)
	if r.at("code") != float64(406) || !strings.Contains(r.raw, "PartialObjectMetadata") {
		t.Fatalf("the 406 must name what was asked for: %s", r.raw)
	}
}

// TestHumanDuration: the boundaries of Kubernetes' humanDuration.
func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{119 * time.Second, "119s"},
		{2 * time.Minute, "2m"},
		{5*time.Minute + 12*time.Second, "5m12s"},
		{9*time.Minute + 59*time.Second, "9m59s"},
		{10 * time.Minute, "10m"},
		{3 * time.Hour, "3h"},
		{7*time.Hour + 59*time.Minute, "7h59m"},
		{8 * time.Hour, "8h"},
		{48 * time.Hour, "2d"},
		{52 * time.Hour, "2d4h"},
		{2 * 365 * 24 * time.Hour, "2y"},
	} {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
