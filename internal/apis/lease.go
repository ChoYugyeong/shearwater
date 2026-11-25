package apis

import (
	"regexp"
	"time"
)

const MicroTimeLayout = "2006-01-02T15:04:05.000000Z"

// Lease timestamps are RFC 3339 strings. Task writers use six fractional
// digits, matching Kubernetes MicroTime's wire representation.
type Lease struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     LeaseSpec  `json:"spec,omitempty"`
}

type LeaseSpec struct {
	HolderIdentity       string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32 `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	RenewTime            string `json:"renewTime,omitempty"`
	LeaseTransitions     *int32 `json:"leaseTransitions,omitempty"`
}

func (l *Lease) Meta() *ObjectMeta { return &l.Metadata }

func (l *Lease) Expiry() time.Time {
	if l.Spec.LeaseDurationSeconds == nil {
		return time.Time{}
	}
	at := l.Spec.RenewTime
	if at == "" {
		at = l.Spec.AcquireTime
	}
	start, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		if at != "" || l.Metadata.CreationTimestamp == nil {
			return time.Time{}
		}
		start = l.Metadata.CreationTimestamp.Time
	}
	return start.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second)
}

// Kubernetes limits a DNS subdomain name to 253 characters in total. A
// conventional lease name can exceed 63 characters after adding the prefix
// to a valid 63-character app name.
var leaseName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func validLeaseName(s string) bool { return len(s) <= 253 && leaseName.MatchString(s) }

func normalizeLeaseTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s // validation reports the field
	}
	return t.UTC().Format(MicroTimeLayout)
}

func (l *Lease) validate() ErrorList {
	var errs ErrorList
	validateMeta(l.Meta(), validLeaseName, nameSubdomain, &errs)
	if n := l.Spec.LeaseDurationSeconds; n != nil && *n < 0 {
		errs.invalid("spec.leaseDurationSeconds", *n, "must be greater than or equal to 0")
	}
	if n := l.Spec.LeaseTransitions; n != nil && *n < 0 {
		errs.invalid("spec.leaseTransitions", *n, "must be greater than or equal to 0")
	}
	for _, field := range []struct{ name, value string }{{"acquireTime", l.Spec.AcquireTime}, {"renewTime", l.Spec.RenewTime}} {
		if field.value != "" {
			if _, err := time.Parse(time.RFC3339Nano, field.value); err != nil {
				errs.invalid("spec."+field.name, field.value, "must be an RFC 3339 timestamp")
			}
		}
	}
	return errs
}

var LeaseResource = &Resource{
	Group: "coordination.k8s.io", Version: "v1", Plural: "leases", Singular: "lease", Kind: "Lease",
	Namespaced: true, Create: true, Update: true, Delete: true,
	New: func() Object { return &Lease{} },
	Default: func(o Object) {
		s := &o.(*Lease).Spec
		s.AcquireTime = normalizeLeaseTime(s.AcquireTime)
		s.RenewTime = normalizeLeaseTime(s.RenewTime)
	},
	Validate: func(o Object) ErrorList { return o.(*Lease).validate() },
}

func init() { Register(LeaseResource) }
