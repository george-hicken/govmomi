package simulator

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
)

// TestAddRoleExtensionCompatibilityPrivileges: vCenter's VirtualMachine.Config.ExtensionCompatibility and
// VirtualMachine.ExtensionCompatibility.Bypass privileges are in the catalogue, so a role that uses them
// (vm-operator's controller role) is added rather than refused with InvalidArgument.
func TestAddRoleExtensionCompatibilityPrivileges(t *testing.T) {
	Test(func(ctx context.Context, c *vim25.Client) {
		m := object.NewAuthorizationManager(c)
		_, err := m.AddRole(ctx, "vmop-ext-compat", []string{
			"VirtualMachine.Config.ExtensionCompatibility",
			"VirtualMachine.ExtensionCompatibility.Bypass",
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
