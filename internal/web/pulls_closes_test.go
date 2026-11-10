package web

import (
	"reflect"
	"testing"
)

func TestClosingRefs(t *testing.T) {
	got := closingRefs("Fixes #3 and closes #12", "resolved: #3, refs #7, Closed #40. prefix#9 fix#8")
	want := []int64{3, 12, 40}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closingRefs = %v, want %v", got, want)
	}
}
