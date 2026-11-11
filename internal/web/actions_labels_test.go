package web

import (
	"reflect"
	"testing"
)

func TestGitHubHostedLabelsMapToAtelier(t *testing.T) {
	got := actDedupeLabels([]string{"ubuntu-latest", "ubuntu-24.04", "macos-14", "windows-2022", "self-hosted", "gpu", "Linux"})
	want := []string{"linux", "macos", "windows", "self-hosted", "gpu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}
