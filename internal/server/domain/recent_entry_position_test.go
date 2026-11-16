package domain

import (
	"testing"
	"time"
)

func TestRecentEntryPositionIsValid(t *testing.T) {
	// 1. 准备数据库可表示的 UTC 两端及带偏移的同一时刻。
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	lowerBound := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
	upperBound := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	eastZone := time.FixedZone("UTC+08", 8*60*60)
	westZone := time.FixedZone("UTC-08", -8*60*60)

	for _, testCase := range []struct {
		name     string
		position RecentEntryPosition
		valid    bool
	}{
		{name: "zero value"},
		{name: "zero ID", position: RecentEntryPosition{LastOpenedAt: stamp}},
		{name: "zero time", position: RecentEntryPosition{ID: 19}},
		{name: "minimum ID", position: RecentEntryPosition{LastOpenedAt: stamp, ID: 1}, valid: true},
		{name: "maximum unsigned ID", position: RecentEntryPosition{LastOpenedAt: stamp, ID: ^uint64(0)}, valid: true},
		{name: "whole second", position: RecentEntryPosition{LastOpenedAt: stamp.Truncate(time.Second), ID: 19}, valid: true},
		{name: "one microsecond", position: RecentEntryPosition{LastOpenedAt: stamp.Truncate(time.Second).Add(time.Microsecond), ID: 19}, valid: true},
		{name: "one nanosecond", position: RecentEntryPosition{LastOpenedAt: stamp.Truncate(time.Second).Add(time.Nanosecond), ID: 19}},
		{name: "below microsecond", position: RecentEntryPosition{LastOpenedAt: stamp.Truncate(time.Second).Add(999 * time.Nanosecond), ID: 19}},
		{name: "extra nanosecond", position: RecentEntryPosition{LastOpenedAt: stamp.Add(time.Nanosecond), ID: 19}},
		{name: "UTC lower bound", position: RecentEntryPosition{LastOpenedAt: lowerBound, ID: 19}, valid: true},
		{name: "UTC upper bound", position: RecentEntryPosition{LastOpenedAt: upperBound, ID: 19}, valid: true},
		{name: "before UTC lower bound", position: RecentEntryPosition{LastOpenedAt: lowerBound.Add(-time.Microsecond), ID: 19}},
		{name: "after UTC upper bound", position: RecentEntryPosition{LastOpenedAt: upperBound.Add(time.Microsecond), ID: 19}},
		{name: "positive offset", position: RecentEntryPosition{LastOpenedAt: stamp.In(eastZone), ID: 19}, valid: true},
		{name: "negative offset", position: RecentEntryPosition{LastOpenedAt: stamp.In(westZone), ID: 19}, valid: true},
		{name: "valid UTC with local year 999", position: RecentEntryPosition{LastOpenedAt: lowerBound.In(westZone), ID: 19}, valid: true},
		{name: "valid UTC with local year 10000", position: RecentEntryPosition{LastOpenedAt: upperBound.In(eastZone), ID: 19}, valid: true},
		{name: "local lower bound outside UTC", position: RecentEntryPosition{LastOpenedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, eastZone), ID: 19}},
		{name: "local upper bound outside UTC", position: RecentEntryPosition{LastOpenedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, westZone), ID: 19}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 校验位置，并核对输入时间、时区与 ID 保留原值。
			original := testCase.position
			if valid := testCase.position.IsValid(); valid != testCase.valid {
				t.Fatalf("位置校验结果为 %v，期望 %v", valid, testCase.valid)
			}
			if testCase.position != original {
				t.Fatal("位置校验修改了输入")
			}
		})
	}
}
