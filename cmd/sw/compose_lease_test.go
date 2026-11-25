package main

import "testing"

func TestComposeLeasePath(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/apis/coordination.k8s.io/v1/namespaces/shop/leases", true},
		{"GET", "/apis/coordination.k8s.io/v1/namespaces/shop/leases/shearwater-app-web", true},
		{"PUT", "/apis/coordination.k8s.io/v1/namespaces/shop/leases/shearwater-app-web", true},
		{"POST", "/apis/coordination.k8s.io/v1/namespaces/shop/leases", false},
		{"DELETE", "/apis/coordination.k8s.io/v1/namespaces/shop/leases/shearwater-app-web", false},
		{"PUT", "/apis/coordination.k8s.io/v1/namespaces/shop/leases/other", false},
		{"GET", "/apis/coordination.k8s.io/v1/leases", false},
		{"GET", "/api/v1/namespaces/shop/pods", false},
	} {
		if got := composeLeasePath(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
}
