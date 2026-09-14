package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	uyuniv1 "github.com/mborodin/uyuni-operator/api/v1alpha1"
	"github.com/mborodin/uyuni-operator/internal/uyuni"
)

// formulaStore is where a set of formula assignments is realized in Uyuni:
// directly on one system, or on a system group (whose formulas and form data
// every member system inherits).
type formulaStore interface {
	assigned(ctx context.Context) ([]string, error)
	assign(ctx context.Context, formulas []string) error
	data(ctx context.Context, formula string) (map[string]any, error)
	setData(ctx context.Context, formula string, data map[string]any) error
}

// serverFormulas is the formulaStore of a single system (Uyuni server ID).
type serverFormulas struct {
	uc uyuni.API
	id int
}

func (s serverFormulas) assigned(ctx context.Context) ([]string, error) {
	return s.uc.GetServerFormulas(ctx, s.id)
}

func (s serverFormulas) assign(ctx context.Context, formulas []string) error {
	return s.uc.SetServerFormulas(ctx, s.id, formulas)
}

func (s serverFormulas) data(ctx context.Context, formula string) (map[string]any, error) {
	return s.uc.GetServerFormulaData(ctx, s.id, formula)
}

func (s serverFormulas) setData(ctx context.Context, formula string, data map[string]any) error {
	return s.uc.SetServerFormulaData(ctx, s.id, formula, data)
}

// groupFormulas is the formulaStore of a system group (Uyuni group ID).
type groupFormulas struct {
	uc uyuni.API
	id int
}

func (g groupFormulas) assigned(ctx context.Context) ([]string, error) {
	return g.uc.GetGroupFormulas(ctx, g.id)
}

func (g groupFormulas) assign(ctx context.Context, formulas []string) error {
	return g.uc.SetGroupFormulas(ctx, g.id, formulas)
}

func (g groupFormulas) data(ctx context.Context, formula string) (map[string]any, error) {
	return g.uc.GetGroupFormulaData(ctx, g.id, formula)
}

func (g groupFormulas) setData(ctx context.Context, formula string, data map[string]any) error {
	return g.uc.SetGroupFormulaData(ctx, g.id, formula, data)
}

// syncFormulas enables formulas on store and reconciles their form data. Form
// data is written only when applyData is true; otherwise resolved data that
// differs from what Uyuni holds is reported as drift. Returns the enabled
// formula names and the drift flag, or a non-empty wait reason if a formula
// isn't installed on the server or a valuesFrom reference isn't available yet.
func syncFormulas(ctx context.Context, c client.Reader, uc uyuni.API, store formulaStore, ns string, formulas []uyuniv1.FormulaAssignment, applyData bool) ([]string, bool, string, error) {
	desired := make([]string, 0, len(formulas))
	for _, f := range formulas {
		desired = append(desired, f.Name)
	}

	if len(desired) > 0 {
		avail, err := uc.ListFormulas(ctx)
		if err != nil {
			return nil, false, "", err
		}
		availSet := map[string]bool{}
		for _, a := range avail {
			availSet[a] = true
		}
		for _, n := range desired {
			if !availSet[n] {
				return nil, false, fmt.Sprintf("formula %q is not installed on the Uyuni server", n), nil
			}
		}
	}

	current, err := store.assigned(ctx)
	if err != nil && !uyuni.IsNotFound(err) {
		return nil, false, "", err
	}
	if add, rm := diffStringSets(current, desired); len(add)+len(rm) > 0 {
		if err := store.assign(ctx, desired); err != nil {
			return nil, false, "", err
		}
	}

	drift := false
	for _, f := range formulas {
		want, wait, err := resolveFormulaValues(ctx, c, ns, f)
		if err != nil {
			return nil, false, "", err
		}
		if wait != "" {
			return nil, false, wait, nil
		}
		if len(want) == 0 {
			continue
		}
		got, err := store.data(ctx, f.Name)
		if err != nil && !uyuni.IsNotFound(err) {
			return nil, false, "", err
		}
		if mapSubset(want, got) {
			continue
		}
		if applyData {
			if err := store.setData(ctx, f.Name, want); err != nil {
				return nil, false, "", err
			}
		} else {
			drift = true
		}
	}
	return desired, drift, "", nil
}

// resolveFormulaValues builds a formula's effective form data: the static
// Values merged with resolved ValuesFromSources (bulk) then ValuesFrom
// (explicit, override). A non-empty wait string means a referenced Secret,
// ConfigMap, or object field isn't available yet.
func resolveFormulaValues(ctx context.Context, c client.Reader, ns string, f uyuniv1.FormulaAssignment) (map[string]any, string, error) {
	data, err := rawExtensionToMap(f.Values)
	if err != nil {
		return nil, "", fmt.Errorf("formula %q values: %w", f.Name, err)
	}
	if data == nil {
		data = map[string]any{}
	}
	for _, src := range f.ValuesFromSources {
		m, wait, err := resolveFormulaBulkSource(ctx, c, ns, src)
		if err != nil || wait != "" {
			return nil, wait, err
		}
		for k, v := range m {
			setPath(data, joinPath(src.Path, k), v)
		}
	}
	for _, vf := range f.ValuesFrom {
		val, wait, err := resolveFormulaValueFrom(ctx, c, ns, vf)
		if err != nil || wait != "" {
			return nil, wait, err
		}
		if vf.Path == "" {
			// Empty path merges a structured (yaml/json) value's top-level keys
			// into the form data root; a scalar has nowhere to go.
			m, ok := val.(map[string]any)
			if !ok {
				return nil, "", fmt.Errorf("formula %q valuesFrom with empty path requires a structured (map) value, got %T", f.Name, val)
			}
			for k, v := range m {
				data[k] = v
			}
			continue
		}
		setPath(data, vf.Path, val)
	}
	return data, "", nil
}

// parseFormulaValue interprets a Secret/ConfigMap string value according to
// format: "" or "string" returns the raw string; "yaml"/"json" deserialize it
// into structured data (nested maps/arrays) with JSON-typed scalars, matching
// how rawExtensionToMap decodes spec.values.
func parseFormulaValue(raw, format string) (any, error) {
	switch format {
	case "", "string":
		return raw, nil
	case "yaml":
		var v any
		if err := sigsyaml.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		return v, nil
	case "json":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func resolveFormulaValueFrom(ctx context.Context, c client.Reader, ns string, vf uyuniv1.FormulaValueFrom) (any, string, error) {
	switch {
	case vf.SecretKeyRef != nil:
		var sec corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: vf.SecretKeyRef.Name}, &sec); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("Secret %q not found (valuesFrom %q)", vf.SecretKeyRef.Name, vf.Path), nil
			}
			return nil, "", err
		}
		data, ok := sec.Data[vf.SecretKeyRef.Key]
		if !ok {
			return nil, fmt.Sprintf("key %q not in Secret %q", vf.SecretKeyRef.Key, vf.SecretKeyRef.Name), nil
		}
		val, err := parseFormulaValue(string(data), vf.Format)
		if err != nil {
			return nil, "", fmt.Errorf("valuesFrom %q: parsing Secret %q key %q as %s: %w", vf.Path, vf.SecretKeyRef.Name, vf.SecretKeyRef.Key, vf.Format, err)
		}
		return val, "", nil
	case vf.ConfigMapKeyRef != nil:
		var cm corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: vf.ConfigMapKeyRef.Name}, &cm); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("ConfigMap %q not found (valuesFrom %q)", vf.ConfigMapKeyRef.Name, vf.Path), nil
			}
			return nil, "", err
		}
		data, ok := cm.Data[vf.ConfigMapKeyRef.Key]
		if !ok {
			return nil, fmt.Sprintf("key %q not in ConfigMap %q", vf.ConfigMapKeyRef.Key, vf.ConfigMapKeyRef.Name), nil
		}
		val, err := parseFormulaValue(data, vf.Format)
		if err != nil {
			return nil, "", fmt.Errorf("valuesFrom %q: parsing ConfigMap %q key %q as %s: %w", vf.Path, vf.ConfigMapKeyRef.Name, vf.ConfigMapKeyRef.Key, vf.Format, err)
		}
		return val, "", nil
	case vf.ObjectFieldRef != nil:
		return resolveObjectFieldRef(ctx, c, ns, vf.ObjectFieldRef)
	}
	return nil, "", nil
}

func resolveFormulaBulkSource(ctx context.Context, c client.Reader, ns string, src uyuniv1.FormulaValueSource) (map[string]any, string, error) {
	out := map[string]any{}
	switch {
	case src.SecretRef != nil:
		var sec corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: src.SecretRef.Name}, &sec); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("Secret %q not found (valuesFromSources)", src.SecretRef.Name), nil
			}
			return nil, "", err
		}
		for k, v := range sec.Data {
			out[k] = string(v)
		}
	case src.ConfigMapRef != nil:
		var cm corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: src.ConfigMapRef.Name}, &cm); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return nil, fmt.Sprintf("ConfigMap %q not found (valuesFromSources)", src.ConfigMapRef.Name), nil
			}
			return nil, "", err
		}
		for k, v := range cm.Data {
			out[k] = v
		}
	}
	return out, "", nil
}

// resolveObjectFieldRef reads a uyuni.uyuni-project.org resource in the same
// namespace and extracts ref.FieldPath (JSONPath). Only the uyuni group is
// allowed — the operator does not read arbitrary cluster resources.
func resolveObjectFieldRef(ctx context.Context, c client.Reader, ns string, ref *uyuniv1.ObjectFieldRef) (any, string, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, "", fmt.Errorf("objectFieldRef apiVersion %q: %w", ref.APIVersion, err)
	}
	if gv.Group != uyuniv1.Group {
		return nil, "", fmt.Errorf("objectFieldRef only supports the %s API group, got %q", uyuniv1.Group, gv.Group)
	}
	var obj unstructured.Unstructured
	obj.SetGroupVersionKind(gv.WithKind(ref.Kind))
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Sprintf("%s %q not found (objectFieldRef)", ref.Kind, ref.Name), nil
		}
		return nil, "", err
	}
	val, err := evalJSONPath(obj.Object, ref.FieldPath)
	if err != nil {
		return nil, "", fmt.Errorf("objectFieldRef fieldPath %q on %s %q: %w", ref.FieldPath, ref.Kind, ref.Name, err)
	}
	if val == nil {
		return nil, fmt.Sprintf("%s %q field %q is empty (not populated yet?)", ref.Kind, ref.Name, ref.FieldPath), nil
	}
	return val, "", nil
}

// evalJSONPath evaluates a JSONPath (without surrounding braces) against a
// decoded object, returning a single value or a slice for multi-match. The path
// is written downward-API style (e.g. "status.imageUrl", no leading dot); k8s
// jsonpath needs a leading "." inside the braces ("{.status.imageUrl}") or it
// treats the first segment as an identifier ("unrecognized identifier status").
func evalJSONPath(obj map[string]any, path string) (any, error) {
	expr := path
	if !strings.HasPrefix(expr, ".") && !strings.HasPrefix(expr, "[") {
		expr = "." + expr
	}
	jp := jsonpath.New("ref").AllowMissingKeys(true)
	if err := jp.Parse("{" + expr + "}"); err != nil {
		return nil, err
	}
	results, err := jp.FindResults(obj)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0]) == 0 {
		return nil, nil
	}
	if len(results[0]) == 1 {
		return results[0][0].Interface(), nil
	}
	out := make([]any, 0, len(results[0]))
	for _, v := range results[0] {
		out = append(out, v.Interface())
	}
	return out, nil
}

// setPath sets value at a dot-separated path in a nested map, creating
// intermediate maps as needed.
func setPath(data map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	m := data
	for i, p := range parts {
		if i == len(parts)-1 {
			m[p] = value
			return
		}
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// rawExtensionToMap unmarshals formula form data into a generic map.
func rawExtensionToMap(raw runtime.RawExtension) (map[string]any, error) {
	if len(raw.Raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw.Raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mapSubset reports whether every key in want is present in got with a deep-equal
// value (recursing into nested maps). Used so Uyuni-injected formula defaults
// don't read as drift.
func mapSubset(want, got map[string]any) bool {
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		if wm, isMap := wv.(map[string]any); isMap {
			gm, ok := gv.(map[string]any)
			if !ok || !mapSubset(wm, gm) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(wv, gv) {
			return false
		}
	}
	return true
}
