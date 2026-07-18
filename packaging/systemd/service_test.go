package systemd_test

import (
	"os"
	"strings"
	"testing"
)

func TestServeAgentServiceHasRootProcessHardening(t *testing.T) {
	contents, err := os.ReadFile("serve-agent.service")
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	unit := string(contents)
	for _, directive := range []string{
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=read-only",
		"PrivateTmp=true",
		"PrivateDevices=true",
		"ProtectKernelTunables=true",
		"ProtectKernelModules=true",
		"ProtectControlGroups=true",
		"RestrictSUIDSGID=true",
		"LockPersonality=true",
		"ReadWritePaths=/var/lib/serve /run/serve /run/docker.sock",
		"UMask=0077",
		"Environment=HOME=/root",
	} {
		if !strings.Contains(unit, directive) {
			t.Errorf("service unit is missing %s", directive)
		}
	}
}
