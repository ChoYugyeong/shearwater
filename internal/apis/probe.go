package apis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Probe is decoded separately because older databases kept the original JSON.
// A probe is an action plus a schedule: readiness removes traffic, whereas
// startup/liveness failures request a graceful container restart.
type Probe struct {
	HTTPGet                       *HTTPProbe `json:"httpGet,omitempty"`
	TCPSocket                     *TCPProbe  `json:"tcpSocket,omitempty"`
	Exec                          *ExecProbe `json:"exec,omitempty"`
	InitialDelaySeconds           int32      `json:"initialDelaySeconds,omitempty"`
	TimeoutSeconds                int32      `json:"timeoutSeconds,omitempty"`
	PeriodSeconds                 int32      `json:"periodSeconds,omitempty"`
	SuccessThreshold              int32      `json:"successThreshold,omitempty"`
	FailureThreshold              int32      `json:"failureThreshold,omitempty"`
	TerminationGracePeriodSeconds *int64     `json:"terminationGracePeriodSeconds,omitempty"`
}
type HTTPProbe struct {
	Path        string       `json:"path,omitempty"`
	Port        IntOrString  `json:"port"`
	Host        string       `json:"host,omitempty"`
	Scheme      string       `json:"scheme,omitempty"`
	HTTPHeaders []HTTPHeader `json:"httpHeaders,omitempty"`
}
type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type TCPProbe struct {
	Port IntOrString `json:"port"`
	Host string      `json:"host,omitempty"`
}
type ExecProbe struct {
	Command []string `json:"command"`
}

func ParseProbe(raw json.RawMessage, kind string, c *Container) (*Probe, error) {
	if rawEmpty(raw) {
		return nil, nil
	}
	p := &Probe{TimeoutSeconds: 1, PeriodSeconds: 10, SuccessThreshold: 1, FailureThreshold: 3}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(p); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one probe object")
	}
	n := 0
	var port *IntOrString
	if p.HTTPGet != nil {
		n++
		h := p.HTTPGet
		port = &h.Port
		if h.Host != "" {
			return nil, fmt.Errorf("httpGet.host is unsupported; probes connect only to the Pod IP; use an HTTP Host header")
		}
		if h.Path == "" {
			h.Path = "/"
		}
		if !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, "\r\n") {
			return nil, fmt.Errorf("HTTP path must start with / and contain no line breaks")
		}
		if h.Scheme == "" {
			h.Scheme = "HTTP"
		}
		if h.Scheme != "HTTP" && h.Scheme != "HTTPS" {
			return nil, fmt.Errorf("scheme must be HTTP or HTTPS")
		}
		for _, v := range h.HTTPHeaders {
			if v.Name == "" || strings.ContainsAny(v.Name, " :\r\n\t") || strings.ContainsAny(v.Value, "\r\n") {
				return nil, fmt.Errorf("invalid HTTP header")
			}
		}
	}
	if p.TCPSocket != nil {
		n++
		port = &p.TCPSocket.Port
		if p.TCPSocket.Host != "" {
			return nil, fmt.Errorf("tcpSocket.host is unsupported; probes connect only to the Pod IP")
		}
	}
	if p.Exec != nil {
		n++
		if len(p.Exec.Command) == 0 || p.Exec.Command[0] == "" {
			return nil, fmt.Errorf("exec.command must be nonempty")
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("exactly one of httpGet, tcpSocket, exec is required (gRPC is unsupported)")
	}
	if port != nil {
		if _, err := ProbePort(*port, c); err != nil {
			return nil, err
		}
	}
	if p.InitialDelaySeconds < 0 || p.InitialDelaySeconds > 86400 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 3600 || p.PeriodSeconds < 1 || p.PeriodSeconds > 86400 || p.SuccessThreshold < 1 || p.SuccessThreshold > 1000 || p.FailureThreshold < 1 || p.FailureThreshold > 1000 {
		return nil, fmt.Errorf("probe schedule outside supported bounds: delay 0..86400, timeout 1..3600, period 1..86400 seconds, thresholds 1..1000")
	}
	if kind != "readiness" && p.SuccessThreshold != 1 {
		return nil, fmt.Errorf("successThreshold must be 1 for %s", kind)
	}
	if g := p.TerminationGracePeriodSeconds; g != nil && (kind == "readiness" || *g < 1 || *g > MaxGraceSeconds) {
		return nil, fmt.Errorf("terminationGracePeriodSeconds requires startup/liveness and 1..%d", MaxGraceSeconds)
	}
	return p, nil
}

func ProbePort(p IntOrString, c *Container) (int, error) {
	if !p.IsString && p.Int >= 1 && p.Int <= 65535 {
		return int(p.Int), nil
	}
	if p.IsString {
		for _, v := range c.Ports {
			if v.Name == p.Str && v.Name != "" && (v.Protocol == "TCP" || v.Protocol == "") {
				return int(v.ContainerPort), nil
			}
		}
	}
	return 0, fmt.Errorf("probe port must be 1..65535 or the name of a TCP port on this container")
}
