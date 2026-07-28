package ard

// PartitionChecks splits a flat verification report into catalog-level checks
// and entry checks keyed by entry slice index.
//
// Attribution is by Check.EntryIndex, which Verify stamps from the position
// the check was actually produced for. This matters because Check.Subject
// carries only an identifier, and a catalog may contain duplicate identifiers
// (itself an identifier.unique violation): the URN string alone cannot say
// which occurrence a given check describes. Resolving Subject through a
// map[identifier]index — the obvious implementation — makes the last entry
// with a given identifier silently absorb every check for all of them, so
// every earlier duplicate is recorded with no checks at all.
//
// Checks that predate the index, or that arrive from a caller constructing
// them by hand, fall back to identifier lookup and are only attributed when
// the identifier is unambiguous. Anything left unresolved is treated as
// catalog-level rather than guessed at, so a check is never attached to an
// entry it does not describe.
func PartitionChecks(checks []Check, entries []Entry) ([]Check, map[int][]Check) {
	// Identifiers appearing more than once are removed from the lookup
	// entirely: they are exactly the ambiguous case that only EntryIndex can
	// resolve, and leaving the first occurrence behind would silently absorb
	// every later duplicate's checks.
	idxByIdentifier := make(map[string]int, len(entries))
	ambiguous := make(map[string]bool)
	for i, entry := range entries {
		if entry.Identifier == "" {
			continue
		}
		if _, seen := idxByIdentifier[entry.Identifier]; seen {
			ambiguous[entry.Identifier] = true
			delete(idxByIdentifier, entry.Identifier)
			continue
		}
		if ambiguous[entry.Identifier] {
			continue
		}
		idxByIdentifier[entry.Identifier] = i
	}

	var catalogChecks []Check
	entryChecks := make(map[int][]Check)

	for _, check := range checks {
		if idx, ok := entryIndexOf(check, idxByIdentifier, ambiguous, len(entries)); ok {
			entryChecks[idx] = append(entryChecks[idx], check)
			continue
		}
		catalogChecks = append(catalogChecks, check)
	}

	return catalogChecks, entryChecks
}

// entryIndexOf resolves the entry a check belongs to.
//
// The identifier is tried first and EntryIndex is consulted only to break a
// duplicate-identifier tie. That ordering is deliberate: EntryIndex's zero
// value is itself a valid entry position, so a check that never had one set
// is indistinguishable from one genuinely stamped for entry 0. Preferring an
// unambiguous identifier match keeps such a check on the entry its Subject
// names, and confines any reliance on the index to the one case identifiers
// provably cannot resolve.
//
// A check whose Subject is SubjectCatalog stays catalog-level even when it
// describes an entry that has no identifier at all — the same entry such a
// check has always rolled up to.
func entryIndexOf(check Check, idxByIdentifier map[string]int, ambiguous map[string]bool, entryCount int) (int, bool) {
	if check.Subject == "" || check.Subject == SubjectCatalog {
		return 0, false
	}
	if idx, ok := idxByIdentifier[check.Subject]; ok {
		return idx, true
	}
	if ambiguous[check.Subject] && check.EntryIndex >= 0 && check.EntryIndex < entryCount {
		return check.EntryIndex, true
	}
	return 0, false
}
