// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"fmt"
	"log"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/vmware/govmomi/vim25/types"
)

// VMRulePhase names the VirtualMachine operation at which a VMRule is evaluated.
type VMRulePhase string

const (
	VMRulePhaseCreate  VMRulePhase = "create"
	VMRulePhasePowerOn VMRulePhase = "powerOn"
)

// VMRule is a match plus an action, applied by vcsim to a VirtualMachine, like a mutating admission
// webhook. Exactly one of Patch and Fault is set.
type VMRule struct {
	// Name identifies the rule in logs, faults and explanations. Required and unique.
	Name string
	// Phases lists where the rule is evaluated. Empty means every phase.
	Phases []VMRulePhase
	// Match selects the VMs the rule applies to. Match.Include must be non-empty.
	Match ObjectMatch
	// Patch is applied after the client's spec. Only ExtraConfig may be set; each value may contain
	// VMNameToken, which is replaced by the VM's name.
	Patch *types.VirtualMachineConfigSpec
	// Fault, when set, fails the operation with a GenericVmConfigFault naming the rule and this text.
	Fault string
}

// VMRules is the ordered rule set. At each phase the first rule that lists the phase and matches is
// applied, and no other. The zero value is ready to use and safe for concurrent use.
type VMRules struct {
	mu    sync.RWMutex
	rules []VMRule
}

// Add appends rule. It fails on an empty or duplicate Name, an empty Match.Include, a LookasideMatch with
// an unknown View or an empty Match, both or neither of Patch and Fault, or a Patch with any field other
// than ExtraConfig set.
func (s *VMRules) Add(rule VMRule) error {
	if err := validateVMRule(rule); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.Name == rule.Name {
			return fmt.Errorf("vcsim: VMRule %q already exists", rule.Name)
		}
	}
	s.rules = append(s.rules, rule)
	return nil
}

func validateVMRule(rule VMRule) error {
	if rule.Name == "" {
		return fmt.Errorf("vcsim: VMRule name is required")
	}
	for _, p := range rule.Phases {
		if p != VMRulePhaseCreate && p != VMRulePhasePowerOn {
			return fmt.Errorf("vcsim: VMRule %q: unknown phase %q", rule.Name, p)
		}
	}
	if len(rule.Match.Include) == 0 {
		return fmt.Errorf("vcsim: VMRule %q: Match.Include is empty", rule.Name)
	}
	for _, l := range rule.Match.Lookaside {
		if _, ok := lookasideViews[l.View]; !ok {
			return fmt.Errorf("vcsim: VMRule %q: unknown lookaside view %q", rule.Name, l.View)
		}
		if len(l.Match) == 0 {
			return fmt.Errorf("vcsim: VMRule %q: lookaside view %q has an empty Match", rule.Name, l.View)
		}
	}
	if (rule.Patch != nil) == (rule.Fault != "") {
		return fmt.Errorf("vcsim: VMRule %q: exactly one of Patch and Fault must be set", rule.Name)
	}
	if rule.Patch != nil {
		patch := *rule.Patch
		patch.ExtraConfig = nil
		if !reflect.ValueOf(patch).IsZero() {
			return fmt.Errorf("vcsim: VMRule %q: Patch may set only ExtraConfig", rule.Name)
		}
	}
	return nil
}

// Remove deletes the rule named name and reports whether it existed.
func (s *VMRules) Remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.rules {
		if r.Name == name {
			s.rules = slices.Delete(s.rules, i, i+1)
			return true
		}
	}
	return false
}

// RuleExplanation says whether one rule applies to a VM at a phase, and why.
type RuleExplanation struct {
	Rule    string
	Phase   VMRulePhase
	Applies bool
	Results []MatchResult
}

// String renders "<rule>: applies|no: key=value want pattern; ...", listing only the keys that did not
// match when Applies is false.
func (e RuleExplanation) String() string {
	if e.Applies {
		return e.Rule + ": applies"
	}
	var parts []string
	for _, r := range e.Results {
		if r.Matched {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v want %s", r.Key, valueOrAbsent(r.Value), r.Want))
	}
	return fmt.Sprintf("%s: no: %s", e.Rule, strings.Join(parts, "; "))
}

func valueOrAbsent(v types.AnyType) any {
	if v == nil {
		return "<absent>"
	}
	return v
}

// ExplainVM evaluates every rule that lists phase against the VM named name, under the VM's lock
// (registryContext(r).WithLock), and returns one explanation per rule in order. It fails with
// fmt.Errorf("vcsim: no VirtualMachine named %q", name) if no VM has that name, and with
// fmt.Errorf("vcsim: %d VirtualMachines named %q", n, name) if more than one has. It must not be called
// from inside a task on the same VM.
func (s *VMRules) ExplainVM(r *Registry, name string, phase VMRulePhase) ([]RuleExplanation, error) {
	var found []*VirtualMachine
	for _, e := range r.All("VirtualMachine") {
		if vm := e.(*VirtualMachine); vm.Name == name {
			found = append(found, vm)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("vcsim: no VirtualMachine named %q", name)
	case 1:
	default:
		return nil, fmt.Errorf("vcsim: %d VirtualMachines named %q", len(found), name)
	}
	vm := found[0]

	s.mu.RLock()
	rules := slices.Clone(s.rules)
	s.mu.RUnlock()

	var out []RuleExplanation
	registryContext(r).WithLock(vm, func() {
		for _, rule := range rules {
			if !rule.listsPhase(phase) {
				continue
			}
			out = append(out, RuleExplanation{
				Rule:    rule.Name,
				Phase:   phase,
				Applies: rule.Match.Matches(r, vm),
				Results: rule.Match.Explain(r, vm),
			})
		}
	})
	return out, nil
}

func (rule VMRule) listsPhase(phase VMRulePhase) bool {
	if len(rule.Phases) == 0 {
		return true
	}
	return slices.Contains(rule.Phases, phase)
}

// apply evaluates the rules for vm at phase and applies the first match. The caller holds vm's lock.
func (s *VMRules) apply(ctx *Context, vm *VirtualMachine, phase VMRulePhase) types.BaseMethodFault {
	s.mu.RLock()
	rules := slices.Clone(s.rules)
	s.mu.RUnlock()

	for _, rule := range rules {
		if !rule.listsPhase(phase) || !rule.Match.Matches(ctx.Map, vm) {
			continue
		}

		if rule.Patch == nil {
			return &types.GenericVmConfigFault{
				Reason: fmt.Sprintf("vcsim VMRule %q (%s) on VM %q: %s", rule.Name, phase, vm.Config.Name, rule.Fault),
			}
		}

		spec := types.VirtualMachineConfigSpec{}
		var keys []string
		for _, o := range rule.Patch.ExtraConfig {
			opt := o.GetOptionValue()
			value, _ := opt.Value.(string)
			spec.ExtraConfig = append(spec.ExtraConfig, &types.OptionValue{
				Key:   opt.Key,
				Value: strings.ReplaceAll(value, VMNameToken, vm.Config.Name),
			})
			keys = append(keys, opt.Key)
		}
		sort.Strings(keys)
		log.Printf("vcsim: VMRule %q (%s) patched VM %q: %s", rule.Name, phase, vm.Config.Name, strings.Join(keys, ","))

		return vm.applyExtraConfig(ctx, &spec)
	}

	if phase == VMRulePhasePowerOn && len(rules) > 0 && !hasExtraConfigKey(vm, "RUN.container") {
		log.Printf("vcsim: no VMRule applied at powerOn; VM %q runs without a container", vm.Config.Name)
	}
	return nil
}

func hasExtraConfigKey(vm *VirtualMachine, key string) bool {
	for _, o := range vm.Config.ExtraConfig {
		if o.GetOptionValue().Key == key {
			return true
		}
	}
	return false
}
