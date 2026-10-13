// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// ObjectMatch selects managed objects by real property values and by lookaside records. Property keys
// follow property.Match (string globs with path.Match, "*" for any non-nil value, any-element match on
// ArrayOf* values, func(any) bool). A path that is absent or nil never matches.
type ObjectMatch struct {
	// Include must match on every key. An empty Include and no Lookaside match every object.
	Include property.Match
	// Lookaside predicates must all hold, in addition to Include.
	Lookaside []LookasideMatch
	// Exclude, when non-empty, stops the match if every one of its keys matches.
	Exclude property.Match
}

// LookasideMatch holds when at least one record of View for the object has, for every key of Match, a
// value named by the key that Match accepts. All keys are checked against the same record.
type LookasideMatch struct {
	View  string
	Match property.Match
}

// LookasideDiskDescriptor is the view of a VirtualMachine's disk descriptors. For each VirtualDisk in
// Config.Hardware.Device order, it has one record for the disk's own descriptor (depth "0") and one for
// each descriptor up its backing Parent chain (depth "1", "2", ...). A record's values are file (the
// datastore path), depth, cid and parentCID (8 hex digits), createType, and "ddb.<key>" for each ddb
// entry; a descriptor that cannot be read gives file, depth and error. Empty values are left out.
const LookasideDiskDescriptor = "diskDescriptor"

// LookasideRecord is one lookaside record: named values, sorted by Name.
type LookasideRecord []types.DynamicProperty

// lookasideViews dispatches lookaside views by name. A view returns false when it does not apply to obj's type.
var lookasideViews = map[string]func(ctx *Context, obj mo.Reference) ([]LookasideRecord, bool){
	LookasideDiskDescriptor: diskDescriptorRecords,
}

// LookasideRecords returns the records of view for obj, empty when it has none. It fails on an unknown
// view or one that does not apply to obj's type. The caller holds obj's lock.
func (r *Registry) LookasideRecords(view string, obj mo.Reference) ([]LookasideRecord, error) {
	return lookasideRecords(registryContext(r), view, obj)
}

func lookasideRecords(ctx *Context, view string, obj mo.Reference) ([]LookasideRecord, error) {
	fn, ok := lookasideViews[view]
	if !ok {
		return nil, fmt.Errorf("vcsim: unknown lookaside view %q", view)
	}
	records, ok := fn(ctx, obj)
	if !ok {
		return nil, fmt.Errorf("vcsim: lookaside view %q does not apply to %s", view, obj.Reference())
	}
	return records, nil
}

// MatchResult is the outcome of one property key, or of one LookasideMatch, on one object.
type MatchResult struct {
	// Key is the property path, or "lookaside:<view>".
	Key     string
	Exclude bool
	// Value is the value seen, nil if absent. For a LookasideMatch it is a types.ArrayOfString, one
	// entry per record, holding the Match's keys as "k=v" sorted by k and joined with ","; a key the
	// record lacks reads "k=<absent>".
	Value types.AnyType
	// Want is the pattern: fmt.Sprint of the property.Match value, or the LookasideMatch's keys as
	// "k=v" sorted by k and joined with ",".
	Want    string
	Matched bool
}

// Matches reports whether obj satisfies m.
func (m ObjectMatch) Matches(r *Registry, obj mo.Reference) bool {
	for key := range m.Include {
		if !m.Include.Property(objectProperty(r, obj, key)) {
			return false
		}
	}
	if len(m.Exclude) > 0 {
		excluded := true
		for key := range m.Exclude {
			if !m.Exclude.Property(objectProperty(r, obj, key)) {
				excluded = false
				break
			}
		}
		if excluded {
			return false
		}
	}
	ctx := registryContext(r)
	for _, l := range m.Lookaside {
		if !l.explain(ctx, obj).Matched {
			return false
		}
	}
	return true
}

// Explain returns Include results sorted by key, then one result per LookasideMatch in order, then
// Exclude results sorted by key.
func (m ObjectMatch) Explain(r *Registry, obj mo.Reference) []MatchResult {
	var results []MatchResult
	for _, key := range sortedKeys(m.Include) {
		prop := objectProperty(r, obj, key)
		results = append(results, MatchResult{
			Key:     key,
			Value:   prop.Val,
			Want:    fmt.Sprint(m.Include[key]),
			Matched: m.Include.Property(prop),
		})
	}

	ctx := registryContext(r)
	for _, l := range m.Lookaside {
		results = append(results, l.explain(ctx, obj))
	}

	for _, key := range sortedKeys(m.Exclude) {
		prop := objectProperty(r, obj, key)
		results = append(results, MatchResult{
			Key:     key,
			Exclude: true,
			Value:   prop.Val,
			Want:    fmt.Sprint(m.Exclude[key]),
			Matched: m.Exclude.Property(prop),
		})
	}

	return results
}

// FaultFilter adapts m, lookaside predicates included, for FaultInjectionRule.InclusionPropertyFilter or
// ExclusionPropertyFilter.
func (m ObjectMatch) FaultFilter(r *Registry) FaultFilterCallback {
	return func(obj mo.Reference) bool {
		return m.Matches(r, obj)
	}
}

// explain evaluates l against obj. An unknown or inapplicable view gives a nil Value and Matched false.
func (l LookasideMatch) explain(ctx *Context, obj mo.Reference) MatchResult {
	res := MatchResult{Key: "lookaside:" + l.View, Want: matchPattern(l.Match)}

	records, err := lookasideRecords(ctx, l.View, obj)
	if err != nil {
		return res
	}

	values := types.ArrayOfString{String: []string{}}
	for _, rec := range records {
		var parts []string
		all := true
		for _, k := range sortedKeys(l.Match) {
			v := recordValue(rec, k)
			if v == nil {
				parts = append(parts, k+"=<absent>")
			} else {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
			if !l.Match.Property(types.DynamicProperty{Name: k, Val: v}) {
				all = false
			}
		}
		values.String = append(values.String, strings.Join(parts, ","))
		if all {
			res.Matched = true
		}
	}

	res.Value = values
	return res
}

func matchPattern(m property.Match) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, ",")
}

func sortedKeys(m property.Match) []string {
	keys := m.Keys()
	sort.Strings(keys)
	return keys
}

func recordValue(rec LookasideRecord, name string) types.AnyType {
	for _, p := range rec {
		if p.Name == name {
			return p.Val
		}
	}
	return nil
}

// objectProperty returns the value of path on obj, as a DynamicProperty named path; Val is nil when
// the path is absent.
func objectProperty(r *Registry, obj mo.Reference, path string) types.DynamicProperty {
	prop := types.DynamicProperty{Name: path}

	var field mo.Field
	if !field.FromString(path) {
		return prop
	}

	rval := getManagedObject(obj)
	keyed := field.Key != nil
	val, err := fieldValue(rval, field.Path, keyed)
	if err == nil && keyed {
		val, err = fieldValueIndex(reflect.ValueOf(val), field)
	}
	if err != nil || val == nil {
		return prop
	}

	if !keyed {
		prop.Val = val
		return prop
	}

	// A keyed ExtraConfig entry: an empty value is removed by applyExtraConfig, so it reads as absent.
	var ov types.OptionValue
	switch x := val.(type) {
	case types.OptionValue:
		ov = x
	case *types.OptionValue:
		if x == nil {
			return prop
		}
		ov = *x
	default:
		return prop
	}
	if s, ok := ov.Value.(string); ok && s != "" {
		prop.Val = s
	}
	return prop
}

// registryContext returns &Context{Context: context.Background(), Map: r}, the minimum context that
// in-package background callers use (datacenter.go:161-165), for callers that have none.
func registryContext(r *Registry) *Context {
	return &Context{Context: context.Background(), Map: r}
}

// diskDescriptorRecords implements LookasideDiskDescriptor. It applies to VirtualMachines only.
func diskDescriptorRecords(ctx *Context, obj mo.Reference) ([]LookasideRecord, bool) {
	vm, ok := obj.(*VirtualMachine)
	if !ok {
		return nil, false
	}
	dc := ctx.Map.getEntityDatacenter(vm)
	if dc == nil || vm.Config == nil {
		return nil, true
	}

	var records []LookasideRecord
	for _, dev := range vm.Config.Hardware.Device {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		for depth, file := range diskBackingChain(disk.Backing) {
			records = append(records, descriptorRecord(ctx, &dc.Self, file, depth))
		}
	}
	return records, true
}

// diskBackingChain returns the file of b, then of each backing Parent up the chain.
func diskBackingChain(b types.BaseVirtualDeviceBackingInfo) []string {
	var files []string
	for b != nil {
		fb, ok := b.(types.BaseVirtualDeviceFileBackingInfo)
		if !ok {
			break
		}
		files = append(files, fb.GetVirtualDeviceFileBackingInfo().FileName)
		b = backingParent(b)
	}
	return files
}

// backingParent returns the Parent field of a disk backing, nil for a backing with none.
func backingParent(b types.BaseVirtualDeviceBackingInfo) types.BaseVirtualDeviceBackingInfo {
	v := reflect.ValueOf(b)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	p := v.Elem().FieldByName("Parent")
	if !p.IsValid() || p.IsNil() {
		return nil
	}
	parent, _ := p.Interface().(types.BaseVirtualDeviceBackingInfo)
	return parent
}

func descriptorRecord(ctx *Context, dc *types.ManagedObjectReference, file string, depth int) LookasideRecord {
	rec := LookasideRecord{
		{Name: "file", Val: file},
		{Name: "depth", Val: strconv.Itoa(depth)},
	}

	desc, _, fault := ctx.Map.FileManager().DiskDescriptor(ctx, dc, file)
	if fault != nil {
		rec = append(rec, types.DynamicProperty{Name: "error", Val: fmt.Sprintf("%v", fault)})
		return rec
	}

	if desc.CID != 0 {
		rec = append(rec, types.DynamicProperty{Name: "cid", Val: desc.CID.String()})
	}
	if desc.ParentCID != 0 {
		rec = append(rec, types.DynamicProperty{Name: "parentCID", Val: desc.ParentCID.String()})
	}
	if desc.Type != "" {
		rec = append(rec, types.DynamicProperty{Name: "createType", Val: desc.Type})
	}
	for k, v := range desc.DDB {
		if v != "" {
			rec = append(rec, types.DynamicProperty{Name: "ddb." + k, Val: v})
		}
	}

	sort.Slice(rec, func(i, j int) bool { return rec[i].Name < rec[j].Name })
	return rec
}
