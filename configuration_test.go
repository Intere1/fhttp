package http_test

import (
	"reflect"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func TestCloneConfiguration(t *testing.T) {
	order := []string{":method", ":scheme", ":authority", ":path"}
	original := &http.Transport{PseudoHeaderOrder: append([]string(nil), order...), ConnectionFlow: 12582912}
	clone := original.Clone()
	if !reflect.DeepEqual(clone.PseudoHeaderOrder, order) || clone.ConnectionFlow != original.ConnectionFlow {
		t.Fatalf("cloned protocol configuration = %v/%d; want %v/%d", clone.PseudoHeaderOrder, clone.ConnectionFlow, order, original.ConnectionFlow)
	}
	original.PseudoHeaderOrder[0] = ":path"
	if !reflect.DeepEqual(clone.PseudoHeaderOrder, order) {
		t.Fatalf("editing original configuration changed clone: %v", clone.PseudoHeaderOrder)
	}
	clone.PseudoHeaderOrder[1] = ":path"
	if original.PseudoHeaderOrder[1] != ":scheme" {
		t.Fatal("editing clone changed original configuration")
	}
	if clone.H2transport != nil {
		t.Fatal("clone shares original runtime connection pool")
	}
}
