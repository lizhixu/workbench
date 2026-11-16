package apps

import (
	"strings"
	"testing"
)

// The one-click Docker installer must not depend on get.docker.com alone:
// mainland-China hosts routinely cannot resolve that name, which turned the
// button into a guaranteed failure. Pin the mirror-first ordering and the
// official fallback so a future edit cannot silently regress it.
func TestDockerInstallScriptMirrorFirst(t *testing.T) {
	script := dockerInstallScript

	for _, mirror := range []string{
		"https://mirrors.aliyun.com/docker-ce",
		"https://mirrors.huaweicloud.com/docker-ce",
		"https://mirrors.tuna.tsinghua.edu.cn/docker-ce",
	} {
		if !strings.Contains(script, mirror) {
			t.Errorf("install script is missing the mirror %s", mirror)
		}
	}
	if !strings.Contains(script, "get.docker.com") {
		t.Error("install script lost the official fallback")
	}

	// The official script must only run after the mirrors failed, otherwise a
	// domestic host still pays the DNS timeout first on every attempt.
	mirrorIdx := strings.Index(script, "install_from_mirrors")
	officialIdx := strings.Index(script, "install_from_official()")
	if mirrorIdx < 0 || officialIdx < 0 {
		t.Fatal("install script is missing the install_from_mirrors/install_from_official functions")
	}
	callMirrors := strings.Index(script, "if ! install_from_mirrors; then")
	callOfficial := strings.Index(script, "if ! install_from_official; then")
	if callMirrors < 0 || callOfficial < 0 {
		t.Fatal("install script does not call the mirror path before the official path")
	}
	if callMirrors > callOfficial {
		t.Error("the official script is attempted before the domestic mirrors")
	}
}

// A failed install must exit non-zero with an actionable message instead of
// reporting success (the old script could not do this: `curl | sh` with a
// DNS failure still left the shell running).
func TestDockerInstallScriptFailsLoudly(t *testing.T) {
	if !strings.Contains(dockerInstallScript, "exit 1") {
		t.Error("install script has no failure exit path")
	}
	if !strings.Contains(dockerInstallScript, "command -v docker") {
		t.Error("install script must verify the docker binary after installing")
	}
	// Both init systems are in scope: systemd on mainstream distros, sysvinit
	// elsewhere (the previous script only called systemctl/service blindly).
	if !strings.Contains(dockerInstallScript, "systemctl") || !strings.Contains(dockerInstallScript, "service docker start") {
		t.Error("install script must start the daemon on both systemd and sysvinit hosts")
	}
}
