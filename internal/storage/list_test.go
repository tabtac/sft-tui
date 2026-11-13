package storage

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sft-tui/internal/protocol"
)

func TestReviewListPaginationMatchesProtocol(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	names := []string{"中文.txt", "résumé", "A", "a", "a' OR 1=1 --", "a b", "z", "é", "文件", "000", "aaa", "aa", "x", "xx", strings.Repeat("m", 255)}
	for i, name := range names {
		reviewPublish(t, s, "alice", name, []byte(name), byte(i+1))
	}
	reviewPublish(t, s, "bob", "private", []byte("secret"), 1)
	reviewCreateTask(t, s, "alice", reviewRequest("unpublished", nil, 99))
	want := append([]string(nil), names...)
	sort.Strings(want)
	for _, limit := range []int{-1, 0, 1, 2, 11, 12, 13, 100} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			effective := limit
			if effective <= 0 || effective > protocol.ListPageSize {
				effective = protocol.ListPageSize
			}
			after := ""
			var got []string
			for page := 0; page <= len(names); page++ {
				files, more, err := s.ListPublishedFiles(context.Background(), "alice", after, limit)
				if err != nil {
					t.Fatal(err)
				}
				if len(files) > effective || (more && len(files) == 0) {
					t.Fatalf("bad page: count=%d more=%v", len(files), more)
				}
				payload, err := protocol.Marshal(protocol.ListResponse{More: more, Files: files})
				if err != nil {
					t.Fatalf("list cannot be encoded: %v", err)
				}
				var decoded protocol.ListResponse
				if err := protocol.Unmarshal(payload, &decoded); err != nil {
					t.Fatal(err)
				}
				for _, file := range files {
					if file.FileName <= after {
						t.Fatalf("cursor did not advance: %q <= %q", file.FileName, after)
					}
					got = append(got, file.FileName)
					after = file.FileName
				}
				if !more {
					break
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("pagination got=%q want=%q", got, want)
			}
		})
	}
}

func TestReviewListCursorAndUserIsolation(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	for i, name := range []string{"a", "c", "e"} {
		reviewPublish(t, s, "alice", name, nil, byte(i+1))
	}
	for _, tc := range []struct {
		user, after string
		want        []string
	}{
		{"alice", "b", []string{"c", "e"}},
		{"alice", "c", []string{"e"}},
		{"alice", "z", nil},
		{"bob", "", nil},
		{"alice' OR 1=1 --", "", nil},
	} {
		t.Run(tc.user+"/"+tc.after, func(t *testing.T) {
			files, more, err := s.ListPublishedFiles(context.Background(), tc.user, tc.after, 12)
			if err != nil || more {
				t.Fatalf("list: more=%v err=%v", more, err)
			}
			var got []string
			for _, file := range files {
				got = append(got, file.FileName)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestReviewListExactPageBoundary(t *testing.T) {
	for _, count := range []int{0, 1, 11, 12, 13, 24} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			for i := 0; i < count; i++ {
				reviewPublish(t, s, "alice", fmt.Sprintf("%02d", i), nil, byte(i+1))
			}
			files, more, err := s.ListPublishedFiles(context.Background(), "alice", "", 12)
			if err != nil || len(files) != min(count, 12) || more != (count > 12) {
				t.Errorf("count=%d returned=%d more=%v err=%v", count, len(files), more, err)
			}
		})
	}
}
