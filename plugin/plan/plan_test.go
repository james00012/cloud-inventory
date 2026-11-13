package plan

import (
	"errors"
	"testing"

	"github.com/james00012/cloud-inventory/plugin/core"
)

func index(order []string, name string) int {
	for i, one := range order {
		if one == name {
			return i
		}
	}
	return -1
}

func TestADeleteRunGoesChildBeforeParent(t *testing.T) {
	order, err := DeleteOrder([]core.TypeInfo{
		{Type: "network"},
		{Type: "subnet", DeleteBefore: []string{"network"}},
		{Type: "instance", DeleteBefore: []string{"subnet", "network"}},
	})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("planned %v", order)
	}
	if index(order, "instance") > index(order, "subnet") {
		t.Fatalf("the instance is deleted after its subnet: %v", order)
	}
	if index(order, "subnet") > index(order, "network") {
		t.Fatalf("the subnet is deleted after its network: %v", order)
	}
}

func TestAPlanIsTheSameEveryTime(t *testing.T) {
	types := []core.TypeInfo{
		{Type: "c"}, {Type: "a", DeleteBefore: []string{"c"}}, {Type: "b", DeleteBefore: []string{"c"}},
	}
	first, err := DeleteOrder(types)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := DeleteOrder(types)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("two plans of the same declaration differ: %v then %v", first, again)
			}
		}
	}
}

func TestADeclarationThatCannotBeOrderedIsRefused(t *testing.T) {
	_, err := DeleteOrder([]core.TypeInfo{
		{Type: "a", DeleteBefore: []string{"b"}},
		{Type: "b", DeleteBefore: []string{"a"}},
	})

	var cycle *Cycle
	if !errors.As(err, &cycle) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if len(cycle.Types) != 2 {
		t.Fatalf("the refusal names %v", cycle.Types)
	}
}

// TestATypeFromAnotherProviderIsNotAnError covers a declaration naming a type
// this provider does not offer, which happens whenever two providers share a
// run.
func TestATypeFromAnotherProviderIsNotAnError(t *testing.T) {
	order, err := DeleteOrder([]core.TypeInfo{
		{Type: "subnet", DeleteBefore: []string{"network-owned-by-another-provider"}},
	})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if len(order) != 1 || order[0] != "subnet" {
		t.Fatalf("planned %v", order)
	}
}
