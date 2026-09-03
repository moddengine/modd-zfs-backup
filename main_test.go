package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseAndValidate(t *testing.T) {
	tests := []struct {
		input string
		want  Source
		bad   bool
	}{
		{"tank/data", Source{Dataset: "tank/data"}, false},
		{"tank/data:daily", Source{Dataset: "tank/data:daily"}, false},
		{"backup@prod.example.com:tank/data", Source{Remote: true, SSHHost: "backup@prod.example.com", Dataset: "tank/data"}, false},
		{"", Source{}, true},
		{"@host:tank/data", Source{}, true},
		{"-user@host:tank/data", Source{}, true},
		{"user@-host:tank/data", Source{}, true},
		{"tank//data", Source{}, true},
		{"tank/data@snap", Source{}, true},
	}
	for _, tt := range tests {
		got, err := parseSource(tt.input)
		if tt.bad && err == nil {
			t.Errorf("parseSource(%q) succeeded", tt.input)
		} else if !tt.bad && (err != nil || !reflect.DeepEqual(got, tt.want)) {
			t.Errorf("parseSource(%q) = %#v, %v; want %#v", tt.input, got, err, tt.want)
		}
	}

	base := Config{Name: "server-a", Source: Source{Dataset: "tank"}, Dest: "backup/server-a"}
	if err := validateConfig(base); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{Name: base.Name, Source: base.Source, Dest: "user@host:tank/data"},
		{Name: base.Name, Source: Source{Dataset: "tank/data"}, Dest: "tank/data"},
		{Name: base.Name, Source: base.Source, Dest: base.Dest, SSHKey: "/key"},
		{Name: base.Name, Source: base.Source, Dest: "tank/backups/server-a", Recursive: true},
		{Name: "bad/name", Source: base.Source, Dest: base.Dest},
		{Name: base.Name, Source: base.Source, Dest: base.Dest, HealthcheckURL: "https://user:pass@example.test/ping"},
	} {
		if err := validateConfig(cfg); err == nil {
			t.Errorf("unsafe config accepted: %#v", cfg)
		}
	}
}

func TestCommandArguments(t *testing.T) {
	cfg := Config{Name: "server-a", Source: Source{Remote: true, SSHHost: "backup@host", Dataset: "tank/data"}, Dest: "backup/server-a", Recursive: true}
	base := &snapshot{Name: "mzb-server-a-old"}
	got := sendArgs(cfg, base, "mzb-server-a-new", true, true)
	want := []string{"send", "-nP", "-w", "-R", "-I", "tank/data@mzb-server-a-old", "tank/data@mzb-server-a-new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("send args: got %#v, want %#v", got, want)
	}
	cfg.SkipIntermediate = true
	if got := sendArgs(cfg, base, "mzb-server-a-new", false, false); got[2] != "-i" {
		t.Fatalf("skip-intermediate flag: got %#v", got)
	}
	ssh := sourceCommand(context.Background(), cfg.Source, "/etc/modd-zfs-backup/key", "list", "tank/data")
	sshArgs := strings.Join(ssh.Args, " ")
	if filepath.Base(ssh.Path) != "ssh" || !strings.Contains(sshArgs, "BatchMode=yes") || !strings.Contains(sshArgs, "IdentitiesOnly=yes -i /etc/modd-zfs-backup/key -- backup@host zfs list tank/data") {
		t.Fatalf("remote command: %#v", ssh.Args)
	}
	recv := receiveArgs(cfg)
	joined := strings.Join(recv, " ")
	for _, required := range []string{"receive -s -u -F", "readonly=on", "canmount=off", "mountpoint=none", ownerNameProp + "=server-a", "backup/server-a"} {
		if !strings.Contains(joined, required) {
			t.Errorf("receive args missing %q: %s", required, joined)
		}
	}
}

func TestCLIFlags(t *testing.T) {
	var output bytes.Buffer
	cfg, source, showVersion, err := parseCLI("modd-zfs-backup", []string{"--name", "test", "--source", "tank/data", "--dest", "backup/data", "--skip-intermediate"}, &output)
	if err != nil || cfg.Name != "test" || source != "tank/data" || !cfg.SkipIntermediate || showVersion {
		t.Fatalf("parseCLI() = %#v, %q, %t, %v", cfg, source, showVersion, err)
	}

	output.Reset()
	_, _, _, err = parseCLI("modd-zfs-backup", []string{"--help"}, &output)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "  --skip-intermediate") || strings.Contains(output.String(), "\n  -skip-intermediate") {
		t.Fatalf("double-dash help: err=%v\n%s", err, output.String())
	}

	for _, args := range [][]string{{"-h"}, {"-name", "test"}, {"--include-intermediate"}} {
		output.Reset()
		if _, _, _, err := parseCLI("modd-zfs-backup", args, &output); err == nil {
			t.Fatalf("parseCLI(%q) accepted removed or single-dash option", args)
		}
	}

	output.Reset()
	_, _, showVersion, err = parseCLI("modd-zfs-backup", []string{"--version"}, &output)
	if err != nil || !showVersion {
		t.Fatalf("--version: show=%t err=%v", showVersion, err)
	}
}

func TestSnapshotAndResumeParsing(t *testing.T) {
	now := time.Unix(1787636071, 656565753)
	if got := snapshotName("fwtest", now, nil); got != "mzb-fwtest-45o2x" {
		t.Fatalf("snapshotName() = %q", got)
	}
	if got := snapshotName("fwtest", now, []snapshot{{Name: "mzb-fwtest-45o2x"}, {Name: "mzb-fwtest-45o2x-1"}}); got != "mzb-fwtest-45o2x-2" {
		t.Fatalf("colliding snapshotName() = %q", got)
	}

	now = time.Now()
	source := []snapshot{{Name: "old", GUID: 1, When: now.Add(-time.Hour)}, {Name: "new", GUID: 2, When: now}}
	dest := []snapshot{{Name: "renamed", GUID: 1, When: now.Add(-time.Hour)}}
	if got := commonSnapshot(source, dest); got == nil || got.GUID != 1 {
		t.Fatalf("common: %#v", got)
	}
	if got := commonSnapshot([]snapshot{{Name: "same", GUID: 1}}, []snapshot{{Name: "same", GUID: 2}}); got != nil {
		t.Fatalf("GUID mismatch selected: %#v", got)
	}
	out := []byte("resume token contents:\ntoname = tank/data@mzb-server-a-123\nsize\t4096\n")
	if got := sendField(out, "toname"); got != "tank/data@mzb-server-a-123" {
		t.Fatalf("toname = %q", got)
	}
	if got, err := parseSendField(out, "size"); err != nil || got != 4096 {
		t.Fatalf("size = %d, %v", got, err)
	}
}

func TestResumeTokenSafety(t *testing.T) {
	cfg := Config{Name: "server-a", Source: Source{Dataset: "tank/data"}}
	snaps := []snapshot{{Name: "mzb-server-a-123", GUID: 1}}
	for _, tt := range []struct {
		name, toname string
		wantOK       bool
	}{
		{"valid", "tank/data@mzb-server-a-123", true},
		{"other-dataset", "tank/other@mzb-server-a-123", false},
		{"other-backup", "tank/data@mzb-other-123", false},
		{"missing-source-snapshot", "tank/data@mzb-server-a-999", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{output: map[string]string{"send -nP -t token": "toname = " + tt.toname + "\nsize = 4096\n"}}
			target, total, err := validateResumeToken(context.Background(), runner, cfg, snaps, "token")
			if tt.wantOK && (err != nil || target == nil || total != 4096) {
				t.Fatalf("valid token rejected: target=%#v total=%d err=%v", target, total, err)
			}
			if !tt.wantOK && err == nil {
				t.Fatalf("unsafe token accepted: target=%#v", target)
			}
		})
	}
}

type fakeRunner struct {
	output map[string]string
	errors map[string]error
	calls  []string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err := f.errors[key]; err != nil {
		return err
	}
	return f.err
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.output[key]), nil
}

func TestCleanupFailuresRemainBestEffort(t *testing.T) {
	runner := &fakeRunner{errors: map[string]error{
		"release mzb-test tank/data@mzb-test-old": errors.New("foreign hold"),
		"destroy tank/data@mzb-test-old":          errors.New("still held"),
	}}
	var logs bytes.Buffer
	cleanup(context.Background(), runner, "mzb-test", "tank/data", []snapshot{{Name: "mzb-test-old"}, {Name: "mzb-test-new"}}, "mzb-test-new", false, "cleanup-source", logger{&logs})
	if len(runner.calls) != 2 || !strings.Contains(logs.String(), " WARN cleanup-source:") || strings.Contains(strings.Join(runner.calls, " "), "mzb-test-new") {
		t.Fatalf("unsafe cleanup behavior: calls=%#v logs=%q", runner.calls, logs.String())
	}
}

func TestFullReplacementReleasesOnlyApplicationHolds(t *testing.T) {
	runner := &fakeRunner{output: map[string]string{
		"holds -H backup/data@mzb-test-owned":    "backup/data@mzb-test-owned\tmzb-test\t1\n",
		"holds -H backup/data@mzb-test-external": "backup/data@mzb-test-external\texternal\t1\n",
	}}
	cfg := Config{Name: "test", Dest: "backup/data", Recursive: true}
	snaps := []snapshot{{Name: "mzb-test-owned"}, {Name: "mzb-test-external"}}
	if err := replaceDestination(context.Background(), runner, cfg, snaps, logger{io.Discard}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "release -r mzb-test backup/data@mzb-test-owned") || strings.Contains(joined, "release -r mzb-test backup/data@mzb-test-external") || !strings.Contains(joined, "destroy -r backup/data") {
		t.Fatalf("replacement calls:\n%s", joined)
	}
}

func TestHoldsAndCleanup(t *testing.T) {
	runner := &fakeRunner{output: map[string]string{
		"holds -H tank/data@mzb-test-new": "tank/data@mzb-test-new\tmzb-test\t1\n",
	}}
	var logs bytes.Buffer
	l := logger{&logs}
	if err := ensureHold(context.Background(), runner, "mzb-test", "tank/data@mzb-test-new", false, "hold-source", l); err != nil {
		t.Fatal(err)
	}
	cleanup(context.Background(), runner, "mzb-test", "tank/data", []snapshot{{Name: "mzb-test-old"}, {Name: "mzb-test-new"}}, "mzb-test-new", false, "cleanup-source", l)
	joined := strings.Join(runner.calls, "\n")
	if strings.Contains(joined, "hold mzb-test tank/data@mzb-test-new") {
		t.Fatal("existing hold was applied twice")
	}
	if !strings.Contains(joined, "release mzb-test tank/data@mzb-test-old") || !strings.Contains(joined, "destroy tank/data@mzb-test-old") || strings.Contains(joined, "destroy tank/data@mzb-test-new") {
		t.Fatalf("cleanup calls:\n%s", joined)
	}
}

func TestLoggerPrefixesMultilineAndCommandErrorIsBounded(t *testing.T) {
	var out bytes.Buffer
	logger{&out}.err("send", "first\nsecond")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], " ERROR send: first") || !strings.Contains(lines[1], " ERROR send: second") {
		t.Fatalf("logs: %q", out.String())
	}
	b := &limitedBuffer{limit: 4}
	n, _ := b.Write([]byte("abcdefgh"))
	if n != 8 || b.String() != "abcd" || !b.truncated {
		t.Fatalf("limited buffer: n=%d value=%q truncated=%t", n, b.String(), b.truncated)
	}
}

func TestLockContention(t *testing.T) {
	t.Setenv("MODD_ZFS_BACKUP_LOCK_DIR", t.TempDir())
	cfg := Config{Name: "test", Source: Source{Dataset: "tank/data"}, Dest: "backup/data"}
	first, acquired, err := acquireLock(lockName(cfg))
	if err != nil || !acquired {
		t.Fatalf("first lock: %t, %v", acquired, err)
	}
	defer first.Close()
	second, acquired, err := acquireLock(lockName(cfg))
	if err != nil || acquired || second != nil {
		t.Fatalf("second lock: %#v, %t, %v", second, acquired, err)
	}
	var logs bytes.Buffer
	if err := execute(context.Background(), cfg, logger{&logs}); err != nil || !strings.Contains(logs.String(), " INFO skip:") {
		t.Fatalf("contended execute: err=%v logs=%q", err, logs.String())
	}

	other, acquired, err := acquireLock(lockName(Config{Name: "test", Source: Source{Dataset: "tank/other"}, Dest: "backup/other"}))
	if err != nil || !acquired {
		t.Fatalf("different datasets share lock: %t, %v", acquired, err)
	}
	other.Close()
}

func TestLockRejectsInvalidPathAndSymlink(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODD_ZFS_BACKUP_LOCK_DIR", file)
	if _, _, err := acquireLock("test"); err == nil {
		t.Fatal("regular file accepted as lock directory")
	}
	if err := execute(context.Background(), Config{Name: "test"}, logger{io.Discard}); err == nil {
		t.Fatal("execute ignored lock directory failure")
	}

	dir := t.TempDir()
	t.Setenv("MODD_ZFS_BACKUP_LOCK_DIR", dir)
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "test.lock")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acquireLock("test"); err == nil {
		t.Fatal("symlink accepted as lock file")
	}
}

func TestHealthcheckLifecycleAndURLPath(t *testing.T) {
	var requests []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		requests = append(requests, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	l := newLogger(io.Discard)
	l.info("test", "backup log")
	for _, suffix := range []string{"start", "", "fail"} {
		if err := pingHealthcheck(context.Background(), "https://example.test/uuid?source=test", suffix, l); err != nil {
			t.Fatal(err)
		}
	}
	if requests[0] != "GET /uuid/start?source=test " || !strings.HasPrefix(requests[1], "POST /uuid?source=test ") || !strings.Contains(requests[1], "INFO test: backup log") || !strings.HasPrefix(requests[2], "POST /uuid/fail?source=test ") || !strings.Contains(requests[2], "INFO test: backup log") {
		t.Fatalf("requests: %#v", requests)
	}
}

func TestHealthcheckFailuresAreWarningsAndFailPingIgnoresCancellation(t *testing.T) {
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	l := logger{io.Discard}

	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})
	if err := pingHealthcheck(context.Background(), "https://example.test/check", "start", l); err == nil {
		t.Fatal("network failure was ignored")
	}

	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := pingHealthcheck(context.Background(), "https://example.test/check", "", l); err == nil {
		t.Fatal("non-2xx response was ignored")
	}

	var path string
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		if r.Context().Err() != nil || r.Body != nil {
			t.Fatalf("failure ping used canceled context or request body")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = failRun(ctx, Config{Name: "test", HealthcheckURL: "https://example.test/check"}, l, time.Now(), "send", "snap", errors.New("failed"), true)
	if path != "/check/fail" {
		t.Fatalf("failure ping path = %q", path)
	}
}

func TestReplicationFailureStepInBothSourceModes(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, failure := range []string{"send", "receive"} {
			t.Run(failure+map[bool]string{false: "-local", true: "-remote"}[remote], func(t *testing.T) {
				dir := t.TempDir()
				zfs := `#!/bin/sh
if [ "$1" = send ]; then
  if [ "$FAILURE" = send ]; then echo 'cannot send snapshot' >&2; exit 7; fi
  exec yes replication-stream
fi
if [ "$1" = receive ]; then
  if [ "$FAILURE" = receive ]; then echo 'cannot receive stream' >&2; exit 8; fi
  cat >/dev/null
fi
`
				ssh := `#!/bin/sh
while [ "$1" != -- ]; do shift; done
shift
shift
shift
exec "$FAKE_ZFS" "$@"
`
				zfsPath := filepath.Join(dir, "zfs")
				if err := os.WriteFile(zfsPath, []byte(zfs), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				t.Setenv("FAKE_ZFS", zfsPath)
				t.Setenv("FAILURE", failure)
				source := Source{Dataset: "tank/data"}
				if remote {
					source.Remote, source.SSHHost = true, "backup@source"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _, err := replicate(ctx, Config{Name: "test", Source: source, Dest: "backup/server-a"}, nil, "snap", "", 0, false, logger{io.Discard})
				var pipeline *pipelineError
				if !errors.As(err, &pipeline) || pipeline.Primary != failure {
					t.Fatalf("got %v, want %s failure", err, failure)
				}
			})
		}
	}
}

func TestReplicationStartFailuresAreAttributed(t *testing.T) {
	t.Run("receive", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, _, err := replicate(context.Background(), Config{Name: "test", Source: Source{Dataset: "tank/data"}, Dest: "backup/data"}, nil, "snap", "", 0, false, logger{io.Discard})
		var pipeline *pipelineError
		if !errors.As(err, &pipeline) || pipeline.Primary != "receive" {
			t.Fatalf("got %v, want receive start failure", err)
		}
	})

	t.Run("send", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte("#!/bin/sh\n[ \"$1\" = receive ] && exec /usr/bin/cat >/dev/null\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ssh"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		cfg := Config{Name: "test", Source: Source{Remote: true, SSHHost: "backup@source", Dataset: "tank/data"}, Dest: "backup/data"}
		_, _, err := replicate(context.Background(), cfg, nil, "snap", "", 0, false, logger{io.Discard})
		var pipeline *pipelineError
		if !errors.As(err, &pipeline) || pipeline.Primary != "send" {
			t.Fatalf("got %v, want send start failure", err)
		}
	})
}
