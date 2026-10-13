// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/types"
)

// ruleVM creates a VM named name with extra ExtraConfig, through the client, and returns its registry object.
func ruleVM(t *testing.T, env *vcSimEnv, name string, extra ...types.BaseOptionValue) (*object.VirtualMachine, *VirtualMachine) {
	t.Helper()
	ref, err := createRuleVMRef(env, name, extra...)
	require.NoError(t, err)
	return object.NewVirtualMachine(env.client.Client, ref), env.simCtx.Map.Get(ref).(*VirtualMachine)
}

func createRuleVMRef(env *vcSimEnv, name string, extra ...types.BaseOptionValue) (types.ManagedObjectReference, error) {
	spec := types.VirtualMachineConfigSpec{
		Name:        name,
		Files:       &types.VirtualMachineFileInfo{VmPathName: "[LocalDS_0] " + name},
		ExtraConfig: extra,
	}
	task, err := env.folder.VmFolder.CreateVM(env.goCtx, spec, env.pool, nil)
	if err != nil {
		return types.ManagedObjectReference{}, err
	}
	info, err := task.WaitForResult(env.goCtx, nil)
	if err != nil {
		return types.ManagedObjectReference{}, err
	}
	return info.Result.(types.ManagedObjectReference), nil
}

// optionValue returns the value of key in vm's ExtraConfig and how many entries have that key.
func optionValue(vm *VirtualMachine, key string) (string, int) {
	var value string
	var n int
	for _, o := range vm.Config.ExtraConfig {
		if ov := o.GetOptionValue(); ov.Key == key {
			value, _ = ov.Value.(string)
			n++
		}
	}
	return value, n
}

// ruleFault returns the GenericVmConfigFault of a failed task, failing the test if err holds another fault.
func ruleFault(t *testing.T, err error) *types.GenericVmConfigFault {
	t.Helper()
	require.Error(t, err)
	var taskErr task.Error
	require.ErrorAs(t, err, &taskErr)
	fault, ok := taskErr.Fault().(*types.GenericVmConfigFault)
	require.True(t, ok, "not a GenericVmConfigFault: %T", taskErr.Fault())
	return fault
}

const testContainerCmd = `["alpine","sh","-c","` + sleepForever + `"]`

func TestVMRulesAddValidation(t *testing.T) {
	valid := VMRule{
		Name:  "valid",
		Match: ObjectMatch{Include: property.Match{"name": "*"}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{&types.OptionValue{Key: "k", Value: "v"}}},
	}
	withRule := func(f func(r *VMRule)) VMRule {
		r := valid
		r.Name = "case"
		f(&r)
		return r
	}

	cases := map[string]VMRule{
		"empty name": withRule(func(r *VMRule) { r.Name = "" }),
		"empty include": withRule(func(r *VMRule) {
			r.Match.Include = nil
		}),
		"unknown lookaside view": withRule(func(r *VMRule) {
			r.Match.Lookaside = []LookasideMatch{{View: "nope", Match: property.Match{"ddb.uuid": "*"}}}
		}),
		"empty lookaside match": withRule(func(r *VMRule) {
			r.Match.Lookaside = []LookasideMatch{{View: LookasideDiskDescriptor}}
		}),
		"patch and fault": withRule(func(r *VMRule) { r.Fault = "no" }),
		"neither patch nor fault": withRule(func(r *VMRule) {
			r.Patch = nil
		}),
		"patch sets NumCPUs": withRule(func(r *VMRule) {
			r.Patch = &types.VirtualMachineConfigSpec{NumCPUs: 2}
		}),
	}

	var rules VMRules
	require.NoError(t, rules.Add(valid))
	cases["duplicate name"] = valid
	for name, rule := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, rules.Add(rule))
		})
	}
}

func TestVMRuleCreatePatch(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:  "patch",
		Match: ObjectMatch{Include: property.Match{"name": "rule-create"}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: "img"},
			&types.OptionValue{Key: "log", Value: "/x/{vmname}"},
		}},
	}))

	_, vm := ruleVM(t, env, "rule-create")
	value, n := optionValue(vm, ContainerBackingOptionKey)
	assert.Equal(t, "img", value)
	assert.Equal(t, 1, n)
	value, _ = optionValue(vm, "log")
	assert.Equal(t, "/x/rule-create", value)
	assert.NotNil(t, vm.svm)
}

func TestVMRulePatchWinsOverClient(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:  "patch",
		Match: ObjectMatch{Include: property.Match{"name": "rule-wins"}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: "k", Value: "rule"},
		}},
	}))

	_, vm := ruleVM(t, env, "rule-wins", &types.OptionValue{Key: "k", Value: "client"})
	value, n := optionValue(vm, "k")
	assert.Equal(t, "rule", value)
	assert.Equal(t, 1, n)
}

func TestVMRuleExcludeKeepsClient(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name: "fill",
		Match: ObjectMatch{
			Include: property.Match{"name": "rule-exclude"},
			Exclude: property.Match{`config.extraConfig["RUN.container"]`: "*"},
		},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: ContainerBackingOptionKey, Value: "rule"},
		}},
	}))

	_, vm := ruleVM(t, env, "rule-exclude", &types.OptionValue{Key: ContainerBackingOptionKey, Value: "c"})
	value, _ := optionValue(vm, ContainerBackingOptionKey)
	assert.Equal(t, "c", value)
}

func TestVMRuleFirstMatchWins(t *testing.T) {
	env := newVCSIMEnv(t)
	match := ObjectMatch{Include: property.Match{"name": "rule-first"}}
	for _, k := range []string{"k1", "k2"} {
		require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
			Name:  "rule-" + k,
			Match: match,
			Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
				&types.OptionValue{Key: k, Value: "v"},
			}},
		}))
	}

	_, vm := ruleVM(t, env, "rule-first")
	_, n1 := optionValue(vm, "k1")
	_, n2 := optionValue(vm, "k2")
	assert.Equal(t, 1, n1)
	assert.Equal(t, 0, n2)
}

func TestVMRulePhases(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:   "power",
		Phases: []VMRulePhase{VMRulePhasePowerOn},
		Match:  ObjectMatch{Include: property.Match{"name": "rule-phases"}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: "k", Value: "v"},
		}},
	}))

	vmObj, vm := ruleVM(t, env, "rule-phases")
	_, n := optionValue(vm, "k")
	assert.Equal(t, 0, n)

	powerOn(t, env, vmObj)
	value, _ := optionValue(vm, "k")
	assert.Equal(t, "v", value)
}

func TestVMRuleNotAtRegisterVM(t *testing.T) {
	env := newVCSIMEnv(t)
	vmObj, vm := ruleVM(t, env, "rule-register")
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:  "create",
		Match: ObjectMatch{Include: property.Match{"name": "rule-register"}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: "k", Value: "v"},
		}},
	}))

	path := vm.Config.Files.VmPathName
	require.NoError(t, vmObj.Unregister(env.goCtx))
	task, err := env.folder.VmFolder.RegisterVM(env.goCtx, path, "", false, env.pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(env.goCtx, nil)
	require.NoError(t, err)

	reg := env.simCtx.Map.Get(info.Result.(types.ManagedObjectReference)).(*VirtualMachine)
	_, n := optionValue(reg, "k")
	assert.Equal(t, 0, n)
}

func TestVMRuleCreateFault(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:  "refuse-create",
		Match: ObjectMatch{Include: property.Match{"name": "fault-create"}},
		Fault: "no create",
	}))

	_, err := createRuleVMRef(env, "fault-create")
	fault := ruleFault(t, err)
	for _, want := range []string{"refuse-create", "create", "fault-create"} {
		assert.Contains(t, fault.Reason, want)
	}

	for _, vm := range env.simCtx.Map.All("VirtualMachine") {
		assert.NotEqual(t, "fault-create", vm.(*VirtualMachine).Name)
	}
}

func TestVMRulePowerOnFault(t *testing.T) {
	env := newVCSIMEnv(t)
	vmObj, vm := ruleVM(t, env, "fault-power")
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:   "refuse-power",
		Phases: []VMRulePhase{VMRulePhasePowerOn},
		Match:  ObjectMatch{Include: property.Match{"name": "fault-power"}},
		Fault:  "no power on",
	}))

	task, err := vmObj.PowerOn(env.goCtx)
	require.NoError(t, err)
	fault := ruleFault(t, task.Wait(env.goCtx))
	assert.Contains(t, fault.Reason, "refuse-power")
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, vm.Runtime.PowerState)
}

func TestVMRulePowerOnPatchIdempotent(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name:   "power-once",
		Phases: []VMRulePhase{VMRulePhasePowerOn},
		Match: ObjectMatch{
			Include: property.Match{"name": "rule-idempotent"},
			Exclude: property.Match{`config.extraConfig["k"]`: "*"},
		},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: "k", Value: "v"},
		}},
	}))

	vmObj, vm := ruleVM(t, env, "rule-idempotent")
	for i := 0; i < 2; i++ {
		powerOn(t, env, vmObj)
		powerOff(t, env, vmObj)
	}
	powerOn(t, env, vmObj)

	_, n := optionValue(vm, "k")
	assert.Equal(t, 1, n)
}

func TestCreateRegistryEmptySpecExtraConfig(t *testing.T) {
	env := newVCSIMEnv(t)
	env.simCtx.Map.ContainerImages.Register(ContainerImageEntry{
		VMNameGlob: "empty-spec",
		OCIImage:   "img",
	})

	_, vm := ruleVM(t, env, "empty-spec")
	assert.NotNil(t, vm.svm)
}

func TestExplainVM(t *testing.T) {
	env := newVCSIMEnv(t)
	require.NoError(t, env.simCtx.Map.VMRules.Add(VMRule{
		Name: "explain",
		Match: ObjectMatch{Include: property.Match{
			"name":                    "explain-vm",
			`config.extraConfig["a"]`: "b",
		}},
		Patch: &types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{Key: "k", Value: "v"},
		}},
	}))
	ruleVM(t, env, "explain-vm")
	reg := env.simCtx.Map

	explanations, err := reg.VMRules.ExplainVM(reg, "explain-vm", VMRulePhaseCreate)
	require.NoError(t, err)
	require.Len(t, explanations, 1)
	assert.False(t, explanations[0].Applies)
	s := explanations[0].String()
	assert.True(t, strings.HasPrefix(s, "explain: no: "), s)
	assert.Contains(t, s, `config.extraConfig["a"]=<absent> want b`)

	_, err = reg.VMRules.ExplainVM(reg, "x", VMRulePhaseCreate)
	assert.EqualError(t, err, `vcsim: no VirtualMachine named "x"`)

	vmNamed(t, reg, "DC0_H0_VM0").Name = "dup"
	vmNamed(t, reg, "DC0_H0_VM1").Name = "dup"
	_, err = reg.VMRules.ExplainVM(reg, "dup", VMRulePhaseCreate)
	assert.EqualError(t, err, `vcsim: 2 VirtualMachines named "dup"`)
}
