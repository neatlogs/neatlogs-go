package main

import (
	"reflect"
	"testing"
)

func TestDiffObjects(t *testing.T) {
	got := diffObjects(map[string]any{"Go": "1.23"}, map[string]any{"Go": "1.24"})
	want := []objectChange{{Key: "Go", Before: "1.23", After: "1.24"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffObjects() = %#v, want %#v", got, want)
	}
}

func TestDiffFiles(t *testing.T) {
	got := diffFiles(
		[]fileInfo{{Path: "old.go", Size: 1}, {Path: "changed.go", Size: 2}},
		[]fileInfo{{Path: "new.go", Size: 1}, {Path: "changed.go", Size: 3}},
	)
	want := fileChanges{
		Added:       []string{"new.go"},
		Removed:     []string{"old.go"},
		SizeChanged: []sizeChange{{Path: "changed.go", BeforeBytes: 2, AfterBytes: 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffFiles() = %#v, want %#v", got, want)
	}
}
