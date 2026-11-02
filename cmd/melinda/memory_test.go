package main

import (
	"runtime/debug"
	"testing"
	"testing/fstest"
)

func TestSoftMemoryLimitIsNineTenthsOfTheContainerLimit(t *testing.T) {
	v2 := fstest.MapFS{"memory.max": {Data: []byte("1073741824\n")}}
	for _, tc := range []struct {
		name    string
		environ []string
		cgroup  fstest.MapFS
		want    int64
	}{
		{"cgroup v2", nil, v2, 966367638},
		{"cgroup v1", nil, fstest.MapFS{"memory/memory.limit_in_bytes": {Data: []byte("536870912\n")}}, 483183819},
		{"empty GOMEMLIMIT", []string{"GOMEMLIMIT="}, v2, 966367638},
		{"GOMEMLIMIT set", []string{"GOMEMLIMIT=300MiB"}, v2, 0},
		{"GOMEMLIMIT off", []string{"GOMEMLIMIT=off"}, v2, 0},
		{"cgroup v2 without a limit", nil, fstest.MapFS{"memory.max": {Data: []byte("max\n")}}, 0},
		{"cgroup v1 without a limit", nil,
			fstest.MapFS{"memory/memory.limit_in_bytes": {Data: []byte("9223372036854771712\n")}}, 0},
		{"no cgroup", nil, fstest.MapFS{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := debug.SetMemoryLimit(-1)
			defer debug.SetMemoryLimit(before)
			if got := limitMemory(tc.environ, tc.cgroup); got != tc.want {
				t.Errorf("limitMemory = %d, want %d", got, tc.want)
			}
			want := tc.want
			if want == 0 {
				want = before
			}
			if got := debug.SetMemoryLimit(-1); got != want {
				t.Errorf("runtime memory limit = %d, want %d", got, want)
			}
		})
	}
}
