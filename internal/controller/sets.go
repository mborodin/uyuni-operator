package controller

import "strconv"

// diffStringSets returns (toAdd, toRemove) such that applying both makes
// existing equal to desired. Order in result is unspecified.
func diffStringSets(existing, desired []string) (toAdd, toRemove []string) {
	exSet := make(map[string]bool, len(existing))
	for _, s := range existing {
		exSet[s] = true
	}
	desSet := make(map[string]bool, len(desired))
	for _, s := range desired {
		desSet[s] = true
	}
	for s := range desSet {
		if !exSet[s] {
			toAdd = append(toAdd, s)
		}
	}
	for s := range exSet {
		if !desSet[s] {
			toRemove = append(toRemove, s)
		}
	}
	return
}

func diffIntSets(existing, desired []int) (toAdd, toRemove []int) {
	exSet := make(map[int]bool, len(existing))
	for _, v := range existing {
		exSet[v] = true
	}
	desSet := make(map[int]bool, len(desired))
	for _, v := range desired {
		desSet[v] = true
	}
	for v := range desSet {
		if !exSet[v] {
			toAdd = append(toAdd, v)
		}
	}
	for v := range exSet {
		if !desSet[v] {
			toRemove = append(toRemove, v)
		}
	}
	return
}

func diffCustomInfo(existing, desired map[string]string) (upsert map[string]string, deleteKeys []string) {
	upsert = make(map[string]string)
	for k, v := range desired {
		if existing[k] != v {
			upsert[k] = v
		}
	}
	for k := range existing {
		if _, want := desired[k]; !want {
			deleteKeys = append(deleteKeys, k)
		}
	}
	return
}

func parseInt(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func intSliceToStrings(ints []int) []string {
	out := make([]string, len(ints))
	for i, v := range ints {
		out[i] = strconv.Itoa(v)
	}
	return out
}
