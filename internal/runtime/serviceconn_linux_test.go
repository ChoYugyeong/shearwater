package runtime

import (
	"bytes"
	"net/netip"
	"testing"
)

func connectionMessage(state byte) []byte {
	orig := nest(1, nest(1, attr(1, []byte{192, 0, 2, 2}), attr(2, []byte{10, 96, 0, 1})), nest(2, attr(1, []byte{6}), attr(2, []byte{123, 45}), attr(3, []byte{0, 80})))
	reply := nest(2, nest(1, attr(1, []byte{10, 244, 0, 5}), attr(2, []byte{192, 0, 2, 2})))
	return bytes.Join([][]byte{{2, 0, 0, 0}, orig, reply, nest(4, nest(1, attr(1, []byte{state}))), attr(12, []byte{0, 0, 0, 42})}, nil)
}

func TestConntrackDrainAndStrictParsing(t *testing.T) {
	pods, svcs := netip.MustParsePrefix("10.244.0.0/24"), netip.MustParsePrefix("10.96.0.0/12")
	for _, state := range []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 255} {
		c, owned, err := parseServiceConnection(connectionMessage(state), pods, svcs)
		if err != nil || !owned || c.active != (state != 7 && state != 8) {
			t.Fatalf("state %d: %+v %v %v", state, c, owned, err)
		}
		attrs, err := checkedAttrs(c.identity)
		if err != nil || !bytes.Equal(attrs[12], []byte{0, 0, 0, 42}) || attrs[1] == nil {
			t.Fatal("delete lacks exact tuple and ID")
		}
	}
	good := connectionMessage(3)
	for n := 0; n < len(good); n++ {
		_, owned, err := parseServiceConnection(good[:n], pods, svcs)
		if err == nil && owned {
			t.Fatalf("accepted truncated dump at %d", n)
		}
	}
	if _, owned, err := parseServiceConnection(good, pods, netip.MustParsePrefix("10.97.0.0/16")); err != nil || owned {
		t.Fatal("foreign connection counted")
	}
}
