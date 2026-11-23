package config

import (
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
)

func TestParseRestoreSyncResetOptionsAcceptsExplicitOptions(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		address    string
		normalized string
		mode       RestoreSyncResetMode
	}{
		{name: "IPv4 initial", address: "127.0.0.1:54123", normalized: "127.0.0.1:54123", mode: RestoreSyncResetInitial},
		{name: "IPv6 resume", address: "[0:0:0:0:0:0:0:1]:54123", normalized: "[::1]:54123", mode: RestoreSyncResetResume},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const roster = "backup files/original roster.json"
			args := []string{"-db-address", fixture.address, "-database=cloudnest_restore_fixture",
				"-roster", roster, "-mode", string(fixture.mode)}
			options, err := ParseRestoreSyncResetOptions(args)
			expected := RestoreSyncResetOptions{
				DatabaseAddress: fixture.normalized, DatabaseName: "cloudnest_restore_fixture", RosterPath: roster, Mode: fixture.mode,
			}
			if err != nil || options != expected {
				t.Fatalf("解析结果为 %+v、%v，预期 %+v", options, err, expected)
			}
		})
	}
}

func TestParseRestoreSyncResetOptionsRequiresExplicitArguments(t *testing.T) {
	t.Setenv("CLOUDNEST_DB_ADDRESS", "127.0.0.1:54123")
	t.Setenv("CLOUDNEST_DB_NAME", "cloudnest_restore_fixture")
	arguments := []string{"-db-address", "127.0.0.1:54123", "-database", "cloudnest_restore_fixture",
		"-roster", "original.json", "-mode", "initial"}
	for index := 0; index < len(arguments); index += 2 {
		name := arguments[index]
		t.Run(name, func(t *testing.T) {
			withoutRequired := slices.Delete(slices.Clone(arguments), index, index+2)
			options, err := ParseRestoreSyncResetOptions(withoutRequired)
			if options != (RestoreSyncResetOptions{}) || err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("缺少 %s 应报错且不返回部分配置，实际为 %+v、%v", name, options, err)
			}
		})
	}
}

func TestParseRestoreSyncResetOptionsRejectsInvalidArguments(t *testing.T) {
	valid := []string{"-db-address", "127.0.0.1:54123", "-database", "cloudnest_restore_fixture", "-roster", "original.json", "-mode", "initial"}
	for _, fixture := range []struct {
		name      string
		replaceAt int
		value     string
		wantInErr string
	}{
		{name: "hostname", replaceAt: 1, value: "localhost:54123", wantInErr: "-db-address"},
		{name: "remote IP", replaceAt: 1, value: "192.0.2.1:54123", wantInErr: "-db-address"},
		{name: "wildcard IP", replaceAt: 1, value: "0.0.0.0:54123", wantInErr: "-db-address"},
		{name: "IPv6 zone", replaceAt: 1, value: "[::1%example]:54123", wantInErr: "-db-address"},
		{name: "missing port", replaceAt: 1, value: "127.0.0.1", wantInErr: "-db-address"},
		{name: "zero port", replaceAt: 1, value: "127.0.0.1:0", wantInErr: "-db-address"},
		{name: "port overflow", replaceAt: 1, value: "127.0.0.1:65536", wantInErr: "-db-address"},
		{name: "blank database", replaceAt: 3, value: " ", wantInErr: "-database"},
		{name: "database whitespace", replaceAt: 3, value: "cloudnest_restore_fixture ", wantInErr: "-database"},
		{name: "blank roster", replaceAt: 5, value: "\t ", wantInErr: "-roster"},
		{name: "unknown mode", replaceAt: 7, value: "restart", wantInErr: "-mode"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			args := slices.Clone(valid)
			args[fixture.replaceAt] = fixture.value
			options, err := ParseRestoreSyncResetOptions(args)
			if options != (RestoreSyncResetOptions{}) || err == nil || !strings.Contains(err.Error(), fixture.wantInErr) {
				t.Fatalf("预期出现 %s 的参数错误，实际为 %+v、%v", fixture.wantInErr, options, err)
			}
		})
	}
	for _, fixture := range []struct {
		name string
		args []string
	}{
		{name: "duplicate database", args: append(slices.Clone(valid), "-database", "another_database")},
		{name: "repeated mode", args: append(slices.Clone(valid), "-mode", "initial")},
		{name: "unknown flag", args: append(slices.Clone(valid), "-unknown")},
		{name: "extra positional argument", args: append(slices.Clone(valid), "--", "extra")},
		{name: "flag missing value", args: []string{"-db-address"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if options, err := ParseRestoreSyncResetOptions(fixture.args); options != (RestoreSyncResetOptions{}) || err == nil {
				t.Fatalf("无法解析的参数应报错，实际为 %+v、%v", options, err)
			}
		})
	}
	if options, err := ParseRestoreSyncResetOptions([]string{"-h"}); options != (RestoreSyncResetOptions{}) || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("帮助应交回命令入口处理，实际为 %+v、%v", options, err)
	}
}
