package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// The commands that act without parsing flags once treated `stop -h` as
// `stop` and stopped the live service. -h must print usage and exit 0 without
// acting; any other argument must be refused with exit 2, also without acting.
func TestNoFlagCommands_HelpAndStrayArgumentsNeverAct(t *testing.T) {
	var acted []string
	oldS, oldP, oldU, oldSt := startFn, stopFn, uninstallFn, statusFn
	startFn = func() error { acted = append(acted, "start"); return nil }
	stopFn = func() error { acted = append(acted, "stop"); return nil }
	uninstallFn = func() error { acted = append(acted, "uninstall"); return nil }
	statusFn = func() (string, error) { acted = append(acted, "status"); return "x", nil }
	t.Cleanup(func() { startFn, stopFn, uninstallFn, statusFn = oldS, oldP, oldU, oldSt })

	cmds := map[string]func([]string) int{
		"start": runStart, "stop": runStop, "uninstall": runUninstall, "status": runStatus,
	}
	for name, run := range cmds {
		for _, h := range []string{"-h", "--help", "-help"} {
			acted = nil
			var code int
			out, errs := captureBoth(t, func() { code = run([]string{h}) })
			if code != 0 || acted != nil {
				t.Errorf("%s %s: exit %d, acted %v", name, h, code, acted)
			}
			if !strings.Contains(out+errs, "usage: claude-memsync "+name) {
				t.Errorf("%s %s printed no usage: %q %q", name, h, out, errs)
			}
		}
		for _, bad := range [][]string{{"--now"}, {"foo"}, {"-h", "extra"}} {
			acted = nil
			var code int
			_, errs := captureBoth(t, func() { code = run(bad) })
			if code != 2 || acted != nil || !strings.Contains(errs, "unexpected argument") {
				t.Errorf("%s %v: exit %d, acted %v, %q", name, bad, code, acted, errs)
			}
		}
	}
}

func TestVersionAndAgeCommands_HelpAndStrayArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVersion(&out, &errb, []string{"-h"}); code != 0 || !strings.Contains(out.String()+errb.String(), "usage: claude-memsync version") {
		t.Errorf("version -h: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runVersion(&out, &errb, []string{"bogus"}); code != 2 || !strings.Contains(errb.String(), "unexpected argument") {
		t.Errorf("version bogus: %d %q", code, errb.String())
	}
	var code int
	// age-filter reads stdin and serves git: -h and stray arguments must not start it.
	o, e := captureBoth(t, func() { code = runAgeFilter([]string{"-h"}) })
	if code != 0 || !strings.Contains(o+e, "usage: claude-memsync age-filter") {
		t.Errorf("age-filter -h: %d %q %q", code, o, e)
	}
	_, e = captureBoth(t, func() { code = runAgeFilter([]string{"x"}) })
	if code != 2 || !strings.Contains(e, "unexpected argument") {
		t.Errorf("age-filter x: %d %q", code, e)
	}
	o, e = captureBoth(t, func() { code = runAgeTextconv([]string{"-h"}) })
	if code != 0 || !strings.Contains(o+e, "usage: claude-memsync age-textconv") {
		t.Errorf("age-textconv -h: %d %q %q", code, o, e)
	}
}

// captureBoth runs f with os.Stdout and os.Stderr redirected.
func captureBoth(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()
	oldO, oldE := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	f()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldO, oldE
	bo, _ := io.ReadAll(ro)
	be, _ := io.ReadAll(re)
	return string(bo), string(be)
}
