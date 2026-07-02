package remote

import (
	"strings"
	"testing"
)

func TestValidateAliasRejectsInjection(t *testing.T) {
	for _, bad := range []string{"", "dev box", "host;rm -rf", "-oProxyCommand=x", "a b", "a$b"} {
		if err := ValidateAlias(bad); err == nil {
			t.Fatalf("ValidateAlias(%q) accepted an unsafe alias", bad)
		}
	}
	for _, good := range []string{"devbox", "dev-box", "dev.box", "dev_box1"} {
		if err := ValidateAlias(good); err != nil {
			t.Fatalf("ValidateAlias(%q) rejected a valid alias: %v", good, err)
		}
	}
}

func TestBootstrapAndForwardArgvAreTokenArrays(t *testing.T) {
	argv, err := BootstrapArgv("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "ssh" || argv[len(argv)-1] != "true" {
		t.Fatalf("bootstrap argv = %v", argv)
	}
	if _, err := BootstrapArgv("bad;alias"); err == nil {
		t.Fatal("BootstrapArgv accepted an unsafe alias")
	}
	fwd, err := ForwardSocketArgv("devbox", "/tmp/local.sock", "/run/remote.sock")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(fwd, " ")
	if !strings.Contains(joined, "-L /tmp/local.sock:/run/remote.sock") {
		t.Fatalf("forward argv = %v", fwd)
	}
}

func TestAutoStartUnitsContainExec(t *testing.T) {
	if !strings.Contains(SystemdUserUnit("agency-supervisor"), "ExecStart=agency-supervisor") {
		t.Fatal("systemd unit missing ExecStart")
	}
	if !strings.Contains(SystemdUserUnit(""), "WantedBy=default.target") {
		t.Fatal("systemd unit missing install section")
	}
	plist := LaunchdAgent("/usr/local/bin/agency-supervisor")
	if !strings.Contains(plist, "com.agency.supervisor") || !strings.Contains(plist, "/usr/local/bin/agency-supervisor") {
		t.Fatal("launchd agent missing label or program")
	}
}

func TestStartAndDiscoverArgv(t *testing.T) {
	start, err := StartArgv("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(start, " ") != "ssh -o BatchMode=yes devbox systemctl --user start agency-supervisor" {
		t.Fatalf("start argv = %v", start)
	}
	disc, err := DiscoverArgv("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if disc[len(disc)-1] != "--json" {
		t.Fatalf("discover argv = %v", disc)
	}
	if _, err := StartArgv("bad;alias"); err == nil {
		t.Fatal("StartArgv accepted unsafe alias")
	}
}
