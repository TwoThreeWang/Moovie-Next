package extapi

import "testing"

func TestTierFor(t *testing.T) {
	cases := map[int]string{0: "", 1: "拾片人", 49: "拾片人", 50: "场记", 999: "资料员", 20000: "影史编修", 99999: "影史编修"}
	for count, want := range cases {
		if got := TierFor(count).Title; got != want {
			t.Errorf("TierFor(%d) = %q，应该是 %q", count, got, want)
		}
	}
}
