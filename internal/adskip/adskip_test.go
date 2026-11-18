package adskip_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"moovie/internal/adskip"
	"moovie/internal/db/testdb"
)

// 这些例子来自广告指纹功能的设计文档，状态判定必须和它一致。
func TestStatusThresholds(t *testing.T) {
	cases := []struct {
		confirm, reject int
		want            string
	}{
		{0, 0, "pending"}, {1, 0, "pending"}, {4, 0, "pending"}, {5, 0, "confirmed"}, {5, 1, "confirmed"},
		{5, 2, "pending"}, {20, 5, "confirmed"}, {3, 5, "rejected"}, {0, 3, "rejected"}, {0, 2, "pending"}, {4, 4, "pending"},
	}
	for _, c := range cases {
		if got := adskip.Status(c.confirm, c.reject); got != c.want {
			t.Errorf("Status(%d, %d) = %q，期望 %q", c.confirm, c.reject, got, c.want)
		}
	}
}

func fingerprint(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestVoteAndLists(t *testing.T) {
	store := adskip.NewStore(testdb.Pool(t))
	ctx := context.Background()
	vote := func(b byte, confirm bool, times int) {
		for range times {
			if err := store.Vote(ctx, fingerprint(b), confirm); err != nil {
				t.Fatal(err)
			}
		}
	}
	vote(0xCC, true, 5)  // 已确认
	vote(0xAA, true, 6)  // 已确认
	vote(0xAA, false, 1) // 仍然已确认（6/7）
	vote(0xBB, false, 3) // 已否认
	vote(0xDD, true, 4)  // 未达门槛

	confirmed, rejected, err := store.Lists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed) != 2 || confirmed[0][0] != 0xAA || confirmed[1][0] != 0xCC || len(confirmed[0]) != adskip.PrefixBytes {
		t.Fatalf("已确认列表应该是排好序的 16 字节前缀: %x", confirmed)
	}
	if len(rejected) != 1 || rejected[0][0] != 0xBB {
		t.Fatalf("已否认列表不对: %x", rejected)
	}
	total, confirmedCount, rejectedCount, err := store.Counts(ctx)
	if err != nil || total != 4 || confirmedCount != 2 || rejectedCount != 1 {
		t.Errorf("统计不对: %d %d %d %v", total, confirmedCount, rejectedCount, err)
	}

	encoded := adskip.Encode(confirmed, rejected)
	if string(encoded[:4]) != "MVFP" || encoded[4] != 1 || encoded[5] != adskip.PrefixBytes {
		t.Fatalf("文件头不对: %x", encoded[:8])
	}
	if binary.BigEndian.Uint32(encoded[8:12]) != 2 || binary.BigEndian.Uint32(encoded[12:16]) != 1 {
		t.Errorf("数量字段不对: %x", encoded[8:16])
	}
	if len(encoded) != 16+3*adskip.PrefixBytes || encoded[16] != 0xAA || encoded[16+2*adskip.PrefixBytes] != 0xBB {
		t.Errorf("指纹数据不对，长度 %d", len(encoded))
	}
}
