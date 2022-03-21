package namespace

import (
	"fmt"
	"sort"
)

// computePermissionAliases computes a map of aliases between the various permissions in a
// namespace. A permission is considered an alias if it *directly* refers to another permission
// or relation without any other form of expression.
func computePermissionAliases(typeSystem *ValidatedNamespaceTypeSystem) (map[string]string, error) {
	aliases := map[string]string{}
	done := map[string]struct{}{}
	workingSet := map[string]string{}

	for _, rel := range typeSystem.nsDef.Relation {
		// Ensure the relation has a rewrite...
		if rel.GetUsersetRewrite() == nil {
			done[rel.Name] = struct{}{}
			continue
		}

		// ... with a union ...
		union := rel.GetUsersetRewrite().GetUnion()
		if union == nil {
			done[rel.Name] = struct{}{}
			continue
		}

		// ... with a single child ...
		if len(union.Child) != 1 {
			done[rel.Name] = struct{}{}
			continue
		}

		// ... that is a computed userset.
		computedUserset := union.Child[0].GetComputedUserset()
		if computedUserset == nil {
			done[rel.Name] = struct{}{}
			continue
		}

		// If the aliased item is a relation, then we've found the alias target.
		aliasedPermOrRel := computedUserset.GetRelation()
		if !typeSystem.IsPermission(aliasedPermOrRel) {
			done[rel.Name] = struct{}{}
			aliases[rel.Name] = aliasedPermOrRel
			continue
		}

		// Otherwise, add the permission to the working set.
		workingSet[rel.Name] = aliasedPermOrRel
	}

	for len(workingSet) > 0 {
		startingCount := len(workingSet)
		for relName, aliasedPermission := range workingSet {
			if _, ok := done[aliasedPermission]; ok {
				done[relName] = struct{}{}

				if alias, ok := aliases[aliasedPermission]; ok {
					aliases[relName] = alias
				} else {
					aliases[relName] = aliasedPermission
				}
				delete(workingSet, relName)
				continue
			}
		}
		if len(workingSet) == startingCount {
			keys := make([]string, 0, len(workingSet))
			for key := range workingSet {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("there exists a cycle in permissions: %v", keys)
		}
	}

	return aliases, nil
}
