// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"fmt"
	"go/types"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis"
)

// CheckInterfaceSatisfaction checks whether concrete types in the loaded
// packages satisfy a given interface. It reports missing methods and
// signature mismatches.
//
// This is not a Template (it doesn't use the per-function Analyze pattern).
// It operates on the full type-checked package set.
func CheckInterfaceSatisfaction(engine *analysis.Engine, typeName, ifaceName string) InterfaceResult {
	result := InterfaceResult{
		TypeName:      typeName,
		InterfaceName: ifaceName,
	}

	// Find the interface.
	ifaceObj := findNamedType(engine, ifaceName)
	if ifaceObj == nil {
		result.Error = fmt.Sprintf("interface %q not found", ifaceName)
		return result
	}
	iface, ok := ifaceObj.Underlying().(*types.Interface)
	if !ok {
		result.Error = fmt.Sprintf("%q is not an interface (is %s)", ifaceName, ifaceObj.Underlying())
		return result
	}

	// Find the concrete type.
	typeObj := findNamedType(engine, typeName)
	if typeObj == nil {
		result.Error = fmt.Sprintf("type %q not found", typeName)
		return result
	}

	// Check both T and *T.
	checkType := typeObj
	if !types.Implements(checkType, iface) {
		checkType = types.NewPointer(typeObj)
	}

	if types.Implements(checkType, iface) {
		result.Satisfies = true
		return result
	}

	// Not satisfied — find missing methods.
	for i := 0; i < iface.NumMethods(); i++ {
		method := iface.Method(i)
		obj, _, _ := types.LookupFieldOrMethod(typeObj, true, nil, method.Name())
		if obj == nil {
			result.Missing = append(result.Missing, MethodInfo{
				Name:      method.Name(),
				Signature: method.Type().String(),
				Reason:    "not implemented",
			})
			continue
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			result.Missing = append(result.Missing, MethodInfo{
				Name:      method.Name(),
				Signature: method.Type().String(),
				Reason:    "found field, not method",
			})
			continue
		}
		// Check signature match.
		if !types.Identical(fn.Type(), method.Type()) {
			result.Missing = append(result.Missing, MethodInfo{
				Name:      method.Name(),
				Signature: method.Type().String(),
				Reason:    fmt.Sprintf("signature mismatch: have %s", fn.Type()),
			})
		}
	}

	return result
}

// InterfaceResult describes whether a type satisfies an interface.
type InterfaceResult struct {
	TypeName      string       `json:"type_name"`
	InterfaceName string       `json:"interface_name"`
	Satisfies     bool         `json:"satisfies"`
	Missing       []MethodInfo `json:"missing,omitempty"`
	Error         string       `json:"error,omitempty"`
}

// MethodInfo describes a missing or mismatched method.
type MethodInfo struct {
	Name      string `json:"name"`
	Signature string `json:"sig"`
	Reason    string `json:"reason"`
}

// findNamedType searches all loaded packages and their imports for a named type.
// Accepts both qualified ("pkg.Type", "io.Reader") and unqualified ("Type") names.
func findNamedType(engine *analysis.Engine, name string) types.Type {
	// Split qualified name: "net/http.Handler" → pkg="net/http", typeName="Handler"
	pkg, typeName := "", name
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		pkg = name[:idx]
		typeName = name[idx+1:]
	}

	// Search directly loaded packages first.
	for _, p := range engine.Packages() {
		if p.Types == nil {
			continue
		}

		if pkg == "" || p.PkgPath == pkg || p.Name == pkg {
			obj := p.Types.Scope().Lookup(typeName)
			if obj != nil {
				if tn, ok := obj.(*types.TypeName); ok {
					return tn.Type()
				}
			}
		}

		// Also search type-checked imports of each loaded package.
		if pkg != "" && p.Types != nil {
			for _, imp := range p.Types.Imports() {
				if imp.Path() == pkg || imp.Name() == pkg {
					obj := imp.Scope().Lookup(typeName)
					if obj != nil {
						if tn, ok := obj.(*types.TypeName); ok {
							return tn.Type()
						}
					}
				}
			}
		}
	}
	return nil
}
