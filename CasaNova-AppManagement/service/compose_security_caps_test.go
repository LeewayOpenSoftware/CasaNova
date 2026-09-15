package service_test

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/compose-spec/compose-go/v2/types"
)

// Pure-value tests: a struct is handed to the validator. No capability is
// requested of any kernel and no container is started.

func capSvc(caps ...string) types.ServiceConfig {
	return types.ServiceConfig{Name: "x", Image: "i", CapAdd: caps}
}

// BPF, PERFMON and SYSLOG are above Docker's default set and hand over kernel
// addresses. None is root in one step — which is why they were easy to leave
// out — but kASLR is what stands between a memory-corruption bug and a working
// exploit, and each of these removes it.
func TestComposeSecurity_StrictImport_BlocksKernelDisclosureCapabilities(t *testing.T) {
	for _, c := range []string{"BPF", "PERFMON", "SYSLOG"} {
		if err := service.ValidateComposeSecurity(mkApp("p", capSvc(c))); err == nil {
			t.Errorf("cap_add: %s was accepted", c)
		}
		// The existing normalisation must cover the new entries too: lowercase
		// and the CAP_ prefix are both how a real compose file spells them.
		for _, spelling := range []string{"cap_" + c, "Cap_" + c} {
			if err := service.ValidateComposeSecurity(mkApp("p", capSvc(spelling))); err == nil {
				t.Errorf("cap_add: %s was accepted — normalisation does not reach the new entries", spelling)
			}
		}
	}
}

// Over-correction guard, and the reason this list is short. Docker grants these
// to EVERY container already, so denying a cap_add for one of them refuses a
// privilege the container holds regardless: a denial that reads like protection
// and grants none, at the price of breaking ordinary apps that spell out what
// they use. Their absence from the denylist is deliberate, and this test is
// what keeps a later "be thorough" pass from adding them.
func TestComposeSecurity_DoesNotBlockCapabilitiesDockerGrantsByDefault(t *testing.T) {
	for _, c := range []string{
		"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
		"NET_BIND_SERVICE", "NET_RAW", "SETFCAP", "SETGID", "SETPCAP",
		"SETUID", "SYS_CHROOT", "AUDIT_WRITE",
	} {
		if err := service.ValidateComposeSecurity(mkApp("p", capSvc(c))); err != nil {
			t.Errorf("cap_add: %s is in Docker's default set; refusing it blocks nothing and breaks apps. Got: %v", c, err)
		}
	}
	// And a service that adds nothing at all is still fine.
	if err := service.ValidateComposeSecurity(mkApp("p", capSvc())); err != nil {
		t.Errorf("a service with no cap_add was refused: %v", err)
	}
}

// The update gate reads the same list, keyed per capability, so one
// pre-existing grant does not smuggle in a second.
func TestComposeSecurity_Update_OneGrantedCapabilityDoesNotCoverAnother(t *testing.T) {
	installed := mkApp("p", capSvc("BPF"))

	if err := service.ValidateComposeSecurityForUpdate(mkApp("p", capSvc("BPF")), installed); err != nil {
		t.Fatalf("an already-installed cap_add must survive an edit, got: %v", err)
	}
	if err := service.ValidateComposeSecurityForUpdate(mkApp("p", capSvc("BPF", "PERFMON")), installed); err == nil {
		t.Fatal("PERFMON was added alongside a pre-existing BPF and passed")
	}
}
