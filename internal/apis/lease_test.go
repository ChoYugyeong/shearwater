package apis

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLeaseValidationAndWireTimes(t *testing.T) {
	res := Lookup("coordination.k8s.io", "v1", "leases")
	if res == nil || !res.Namespaced || res.Kind != "Lease" || !res.Create || !res.Update || !res.Delete {
		t.Fatal("missing Lease registration", res)
	}
	long := &Lease{Metadata: ObjectMeta{Name: "shearwater-app-" + strings.Repeat("a", 63)}}
	if errs := res.Validate(long); len(errs) != 0 {
		t.Fatal("lease for a maximum-length app name", errs)
	}
	for _, tc := range []struct {
		spec  string
		field string
	}{
		{`{}`, ""},
		{`{"leaseDurationSeconds":0,"leaseTransitions":0}`, ""},
		{`{"holderIdentity":"a","leaseDurationSeconds":2,"leaseTransitions":1,"acquireTime":"2026-11-25T01:02:03.123456Z","renewTime":"2026-11-25T01:02:03.654321Z"}`, ""},
		{`{"leaseDurationSeconds":-1}`, "spec.leaseDurationSeconds"},
		{`{"leaseTransitions":-1}`, "spec.leaseTransitions"},
		{`{"renewTime":"yesterday"}`, "spec.renewTime"},
		{`{"acquireTime":"12:00"}`, "spec.acquireTime"},
	} {
		l := &Lease{Metadata: ObjectMeta{Name: "web", Namespace: "default"}}
		if err := json.Unmarshal([]byte(tc.spec), &l.Spec); err != nil {
			t.Fatal(err)
		}
		errs := res.Validate(l)
		if tc.field == "" {
			if len(errs) != 0 {
				t.Fatal(tc.spec, errs)
			}
		} else if len(errs) != 1 || errs[0].Field != tc.field {
			t.Fatal(tc.spec, errs)
		}
		if l.Spec.HolderIdentity == "a" && l.Expiry().Format(time.RFC3339Nano) != "2026-11-25T01:02:05.654321Z" {
			t.Fatal("microseconds lost", l.Expiry())
		}
	}
}
