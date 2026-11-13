package apis

import (
	"encoding/json"
	"testing"
)

func TestProbeValidationAndDefaults(t *testing.T) {
	c := &Container{Ports: []ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: "TCP"}}}
	p, err := ParseProbe(json.RawMessage(`{"httpGet":{"port":"http"}}`), "readiness", c)
	if err != nil || p.TimeoutSeconds != 1 || p.PeriodSeconds != 10 || p.FailureThreshold != 3 || p.HTTPGet.Path != "/" {
		t.Fatal(p, err)
	}
	for _, raw := range []string{`{"exec":{"command":[]}}`, `{"grpc":{"port":80}}`, `{"tcpSocket":{"port":"missing"}}`, `{"httpGet":{"port":80,"host":"127.0.0.1"}}`, `{"httpGet":{"port":80},"exec":{"command":["true"]}}`, `{"exec":{"command":["true"]},"periodSeconds":0}`, `{"exec":{"command":["true"]},"successThreshold":2}`, `{"httpGet":{"port":80,"httpHeaders":[{"name":"X-Test","value":"a\r\nb"}]}}`} {
		if _, err := ParseProbe(json.RawMessage(raw), "liveness", c); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}
