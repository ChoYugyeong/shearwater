package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shearwater/internal/pki"
)

// Use a real CA-verified TLS connection and real CLI parsing. Fixtures may
// answer only GET: these diagnostic commands have no path to a mutation.
func diagnosticFixture(t *testing.T, handler http.HandlerFunc) []string {
	t.Helper()
	material, err := pki.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range map[string][]byte{"ca.crt": material.CACert, "admin.token": []byte(material.Token)} {
		if err = os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.TLS == nil || r.Header.Get("Authorization") != "Bearer "+material.Token {
			t.Error("request is not an authenticated HTTPS GET")
			w.WriteHeader(403)
			return
		}
		handler(w, r)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{material.Server}, MinVersion: tls.VersionTLS12}
	s.StartTLS()
	t.Cleanup(s.Close)
	return []string{"--config-dir", dir, "--listen", s.Listener.Addr().String()}
}

func diagnosticPod(uid, revision string, restarts int) string {
	return fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web","namespace":"demo","uid":%q,"resourceVersion":%q},"spec":{"nodeName":"test","containers":[{"name":"app","image":"busybox"}]},"status":{"phase":"Running","containerStatuses":[{"name":"app","image":"busybox","ready":true,"restartCount":%d,"containerID":"containerd://%s-app-%d","state":{"running":{}},"lastState":{"terminated":{"exitCode":7,"reason":"Error"}}}]}}`, uid, revision, restarts, uid, restarts)
}

func TestDiagnosticFlagsAndReferences(t *testing.T) {
	for _, args := range [][]string{{"pod/web", "-n", "demo", "--full"}, {"--full", "-n=demo", "pod/web"}} {
		o, _, err := parseDiagnostic("get", args, &bytes.Buffer{})
		if err != nil || !o.full || o.namespace != "demo" || o.ref.name != "web" {
			t.Fatalf("%v: %#v %v", args, o, err)
		}
	}
	for _, tt := range []struct {
		command string
		args    []string
	}{
		{"get", []string{"pod/../web"}}, {"get", []string{"pod/web/log"}}, {"get", []string{"pods", "-n", "bad/ns"}},
		{"get", []string{"pods", "-l", "app in ("}}, {"get", []string{"pod/web", "-A"}}, {"get", []string{"pod/web", "-l", "app=web"}},
		{"logs", []string{"deploy/web"}}, {"logs", []string{"pod/web", "--tail=-1"}}, {"logs", []string{"pod/web", "--limit-bytes=0"}},
		{"logs", []string{"pod/web", "--tail=10001"}}, {"why", []string{"pod/web", "--timeout=0s"}}, {"why", []string{"pod/web", "--limit=0"}},
		{"events", []string{"pod/web"}}, {"events", []string{"--since=-1"}}, {"events", []string{"--since=9223372036854775808"}},
		{"events", []string{"--for=pod/web", "-A"}}, {"get", []string{"pods", "--wat"}}, {"get", []string{"pods", "-n"}},
	} {
		t.Run(tt.command+strings.Join(tt.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			err := runDiagnostic(tt.command, tt.args, &out)
			assertInspectError(t, err, "INVALID", "never")
			if out.Len() != 0 {
				t.Fatal("partial output")
			}
		})
	}
	for _, command := range []string{"get", "logs", "events", "why"} {
		var out bytes.Buffer
		if err := runDiagnostic(command, []string{"-h"}, &out); err != nil || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("help %s: %s %v", command, out.String(), err)
		}
	}
}

func TestGetCompactAndFullJSON(t *testing.T) {
	flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/apps/v1/namespaces/demo/deployments" || r.URL.Query().Get("labelSelector") != "app=web" {
			t.Errorf("wrong route %s", r.URL)
		}
		fmt.Fprint(w, `{"kind":"DeploymentList","metadata":{"resourceVersion":"6"},"items":[{"kind":"Deployment","metadata":{"name":"web","namespace":"demo","uid":"d","resourceVersion":"6","generation":9007199254740993},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"nginx","env":[{"name":"SECRET","value":"private-value"}]}]}}},"status":{"readyReplicas":1,"replicas":2}}]}`)
	})
	for _, full := range []bool{false, true} {
		args := append([]string{"deploy", "-n", "demo", "-l", "app=web"}, flags...)
		if full {
			args = append(args, "--full")
		}
		var out bytes.Buffer
		if err := runDiagnostic("get", args, &out); err != nil {
			t.Fatal(err)
		}
		if full {
			if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "private-value") || !strings.Contains(out.String(), "9007199254740993") {
				t.Fatal(out.String())
			}
		} else if !strings.Contains(out.String(), "1/2 ready") || strings.Contains(out.String(), "private-value") {
			t.Fatal(out.String())
		}
	}
}

// A diagnostic must not combine a new Pod with the old Pod's logs/events.
// Force a real identity change between HTTP reads, including on every retry.
func TestDiagnosticEvidenceRejectsChangingIdentity(t *testing.T) {
	for _, command := range []string{"events", "why"} {
		t.Run(command, func(t *testing.T) {
			reads := 0
			flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/events"):
					fmt.Fprint(w, `{"metadata":{"resourceVersion":"100"},"items":[]}`)
				case strings.HasSuffix(r.URL.Path, "/log"):
					fmt.Fprint(w, "must not escape to stdout\n")
				default:
					reads++
					fmt.Fprint(w, diagnosticPod(fmt.Sprint(reads), fmt.Sprint(reads), 0))
				}
			})
			args := []string{"pod/web", "-n=demo"}
			if command == "events" {
				args[0] = "--for=pod/web"
			}
			var out bytes.Buffer
			err := runDiagnostic(command, append(args, flags...), &out)
			assertInspectError(t, err, "CONFLICT", "safe")
			if out.Len() != 0 || reads > 6 {
				t.Fatalf("unstable evidence leaked or retried without a bound: reads=%d output=%q", reads, out.String())
			}
		})
	}
}

func TestLogsRoutingPreviousAndNoPartialOnIdentityChange(t *testing.T) {
	for _, mode := range []string{"same", "recreated", "restarted"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/log") {
					q := r.URL.Query()
					if q.Get("previous") != "true" || q.Get("container") != "app" || q.Get("tailLines") != "2" || q.Get("limitBytes") != "100" {
						t.Error(q)
					}
					fmt.Fprint(w, "old attempt\n")
					return
				}
				reads++
				uid, revision, restarts := "old", "7", 1
				if reads > 1 && mode == "recreated" {
					uid = "new"
				}
				if reads > 1 && mode == "restarted" {
					restarts++
				}
				fmt.Fprint(w, diagnosticPod(uid, revision, restarts))
			})
			var out bytes.Buffer
			err := runDiagnostic("logs", append([]string{"pod/web", "-n", "demo", "--previous", "--tail=2", "--limit-bytes=100"}, flags...), &out)
			if mode == "same" {
				if err != nil || out.String() != "old attempt\n" {
					t.Fatalf("%q %v", out.String(), err)
				}
			} else {
				assertInspectError(t, err, "CONFLICT", "safe")
				if out.Len() != 0 {
					t.Fatal("wrong attempt leaked to stdout")
				}
			}
		})
	}
}

func TestEventsUseUIDSinceLimitAndEscapeControls(t *testing.T) {
	flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pods/web") {
			fmt.Fprint(w, diagnosticPod("new", "10", 0))
			return
		}
		if q := r.URL.Query().Get("fieldSelector"); q != "" && q != "involvedObject.uid=new" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `{"metadata":{"resourceVersion":"12"},"items":[{"metadata":{"name":"old","resourceVersion":"12"},"involvedObject":{"uid":"old"},"message":"old Pod data"},{"metadata":{"name":"a","resourceVersion":"8"},"involvedObject":{"uid":"new"},"message":"before cursor"},{"metadata":{"name":"b","resourceVersion":"10"},"involvedObject":{"uid":"new"},"message":"older"},{"metadata":{"name":"c","resourceVersion":"11"},"involvedObject":{"uid":"new"},"message":"latest\n\u001b[31m"}]}`)
	})
	var out bytes.Buffer
	err := runDiagnostic("events", append([]string{"-n", "demo", "--for=pod/web", "--since=8", "--limit=1"}, flags...), &out)
	if err != nil || !strings.Contains(out.String(), "latest") || strings.Contains(out.String(), "old Pod data") || strings.ContainsRune(out.String(), '\x1b') || !strings.Contains(out.String(), "1 older matching") {
		t.Fatalf("%q %v", out.String(), err)
	}
	out.Reset()
	err = runDiagnostic("events", append([]string{"--since=13"}, flags...), &out)
	assertInspectError(t, err, "INVALID", "never")
}

func TestWhyPreservesMissingEvidenceAndExitDetails(t *testing.T) {
	flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("previous") == "true" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"message":"previous log not found"}`)
			} else {
				fmt.Fprint(w, "app marker\n\x1b[31munsafe control\n")
			}
		case strings.HasSuffix(r.URL.Path, "/events"):
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"7"},"items":[]}`)
		default:
			fmt.Fprint(w, diagnosticPod("p", "7", 1))
		}
	})
	var out bytes.Buffer
	if err := runDiagnostic("why", append([]string{"pod/web", "-n=demo"}, flags...), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"previous exit=7", "app marker", "unavailable:", "previous log not found", "Configured readiness"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Fatal("raw control sequence in diagnosis")
	}
	out.Reset()
	if err := runDiagnostic("why", append([]string{"pod/web", "-n=demo", "-o=json"}, flags...), &out); err != nil || !json.Valid(out.Bytes()) {
		t.Fatalf("json %q %v", out.String(), err)
	}
}

func TestDiagnosticHTTPErrorsAndDeadline(t *testing.T) {
	for status, code := range map[int]string{400: "INVALID", 401: "UNAUTHORIZED", 403: "UNAUTHORIZED", 404: "NOT_FOUND", 409: "CONFLICT", 501: "UNSUPPORTED", 500: "RUNTIME_ERROR"} {
		t.Run(code+fmt.Sprint(status), func(t *testing.T) {
			flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"fixture error"}`)
			})
			var out bytes.Buffer
			err := runDiagnostic("get", append([]string{"pod/web"}, flags...), &out)
			retry := "never"
			if status == 409 || status == 500 {
				retry = "safe"
			}
			assertInspectError(t, err, code, retry)
			if out.Len() != 0 {
				t.Fatal("error wrote stdout")
			}
		})
	}
	flags := diagnosticFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	var out bytes.Buffer
	err := runDiagnostic("get", append([]string{"pod/web", "--timeout=20ms"}, flags...), &out)
	assertInspectError(t, err, "TIMEOUT", "safe")
}
