// Package plan orders a delete run from what a provider declared. The provider
// says which types must go before which; the tool decides the order and runs
// it, which is the split the contract draws.
package plan

import (
	"fmt"
	"sort"

	"github.com/james00012/cloud-inventory/plugin/core"
)

// Cycle is two or more types that each have to be deleted before the other, so
// no order satisfies them. A provider that declares one has a bug, and a tool
// should refuse rather than pick.
type Cycle struct{ Types []string }

func (c *Cycle) Error() string {
	return fmt.Sprintf("these types must each be deleted before the other: %v", c.Types)
}

// DeleteOrder sorts the declared types so that every type comes before the ones
// it must be deleted before. Types with no relation keep a stable order, so two
// runs of the same declaration plan the same way.
func DeleteOrder(types []core.TypeInfo) ([]string, error) {
	known := map[string]bool{}
	for _, one := range types {
		known[one.Type] = true
	}

	// after[t] is what must wait for t. before[t] counts what t waits on.
	after := map[string][]string{}
	waiting := map[string]int{}
	for _, one := range types {
		if _, ok := waiting[one.Type]; !ok {
			waiting[one.Type] = 0
		}
		for _, later := range one.DeleteBefore {
			if !known[later] {
				// A declaration naming a type this provider does not offer is
				// not an error: another provider may own it.
				continue
			}
			after[one.Type] = append(after[one.Type], later)
			waiting[later]++
		}
	}

	var ready []string
	for name, count := range waiting {
		if count == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)

		var freed []string
		for _, later := range after[next] {
			waiting[later]--
			if waiting[later] == 0 {
				freed = append(freed, later)
			}
		}
		sort.Strings(freed)
		ready = append(ready, freed...)
		sort.Strings(ready)
	}

	if len(order) != len(waiting) {
		var stuck []string
		for name, count := range waiting {
			if count > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)
		return nil, &Cycle{Types: stuck}
	}
	return order, nil
}
