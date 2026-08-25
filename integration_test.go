//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	integrationSourcePool = "mzbsource"
	integrationDestPool   = "mzbbackup"
	faultFile             = "/run/modd-zfs-backup-test/fault"
	commandLog            = "/run/modd-zfs-backup-test/commands.log"
	processLog            = "/run/modd-zfs-backup-test/processes.log"
	faultState            = "/run/modd-zfs-backup-test/fault-state"
)

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("MZB_INTEGRATION") != "1" {
		t.Skip("set MZB_INTEGRATION=1 inside the disposable ZFS VM")
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration tests must run as root inside the disposable VM")
	}
	t.Setenv("MODD_ZFS_BACKUP_LOCK_DIR", t.TempDir())
	_ = os.Remove(faultFile)
	_ = os.Remove(faultState)
	_ = os.WriteFile(commandLog, nil, 0666)
	_ = os.WriteFile(processLog, nil, 0666)
}

func realZFS(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/sbin/zfs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zfs %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func zfsMayFail(args ...string) {
	_ = exec.Command("/usr/sbin/zfs", args...).Run()
}

func resetDatasets(t *testing.T, name string) (string, string, string) {
	t.Helper()
	if !nameRE.MatchString(name) {
		t.Fatalf("unsafe test dataset name %q", name)
	}
	source, dest := integrationSourcePool+"/"+name, integrationDestPool+"/"+name
	zfsMayFail("destroy", "-r", dest)
	zfsMayFail("destroy", "-r", source)
	mount := filepath.Join("/mnt/mzbsource", name)
	realZFS(t, "create", "-o", "mountpoint="+mount, source)
	t.Cleanup(func() {
		_ = os.Remove(faultFile)
		zfsMayFail("destroy", "-r", dest)
		zfsMayFail("destroy", "-r", source)
	})
	return source, dest, mount
}

func integrationConfig(name, source, dest string, remote bool, healthURL string) Config {
	s := Source{Dataset: source}
	if remote {
		s.Remote, s.SSHHost = true, "mzbsource@127.0.0.1"
	}
	return Config{Name: name, Source: s, Dest: dest, HealthcheckURL: healthURL}
}

type healthEvents struct {
	mu     sync.Mutex
	paths  []string
	server *httptest.Server
}

func newHealthEvents(t *testing.T) *healthEvents {
	t.Helper()
	h := &healthEvents{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.Path)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *healthEvents) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

func withInterval(t *testing.T, value time.Duration) {
	t.Helper()
	old := minimumInterval
	minimumInterval = value
	t.Cleanup(func() { minimumInterval = old })
}

func matchingSnapshots(t *testing.T, dataset, name string) []snapshot {
	t.Helper()
	snaps, err := listSnapshots(context.Background(), localZFS{}, dataset, name)
	if err != nil {
		t.Fatal(err)
	}
	return snaps
}

func writeData(t *testing.T, mount, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(mount, name), []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}

func sourceMode(remote bool) string {
	if remote {
		return "remote"
	}
	return "local"
}

func setFault(t *testing.T, fault string) {
	t.Helper()
	_ = os.Remove(faultState)
	if err := os.WriteFile(faultFile, []byte(fault), 0666); err != nil {
		t.Fatal(err)
	}
}

func clearFault(t *testing.T) {
	t.Helper()
	if err := os.Remove(faultFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(faultState)
}

func assertRecordedProcessesExited(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(processLog)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var alive []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			fields := strings.SplitN(line, "|", 2)
			pid, parseErr := strconv.Atoi(fields[0])
			if parseErr == nil && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				alive = append(alive, line)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline processes still alive after cancellation: %q", alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func assertProtected(t *testing.T, cfg Config) snapshot {
	t.Helper()
	source := matchingSnapshots(t, cfg.Source.Dataset, cfg.Name)
	dest := matchingSnapshots(t, cfg.Dest, cfg.Name)
	if len(source) != 1 || len(dest) != 1 || source[0].GUID != dest[0].GUID {
		t.Fatalf("unexpected snapshots: source=%#v destination=%#v", source, dest)
	}
	tag := "mzb-" + cfg.Name
	for _, path := range []string{cfg.Source.Dataset + "@" + source[0].Name, cfg.Dest + "@" + dest[0].Name} {
		held, err := hasHold(context.Background(), localZFS{}, path, tag)
		if err != nil || !held {
			t.Fatalf("snapshot %s is not protected: held=%t err=%v", path, held, err)
		}
	}
	return source[0]
}

func TestIntegrationFullIncrementalSkipBothModes(t *testing.T) {
	requireIntegration(t)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run(mode, func(t *testing.T) {
			withInterval(t, 0)
			name := "basic-" + mode
			source, dest, mount := resetDatasets(t, name)
			health := newHealthEvents(t)
			cfg := integrationConfig(name, source, dest, remote, health.server.URL+"/check")
			writeData(t, mount, "one", "first")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			writeData(t, mount, "two", "second")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			last := assertProtected(t, cfg)
			before := len(health.snapshot())
			minimumInterval = 5 * time.Minute
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			if len(health.snapshot()) != before || matchingSnapshots(t, source, name)[0].GUID != last.GUID {
				t.Fatal("protected young snapshot did not skip quietly")
			}
			commands, _ := os.ReadFile(commandLog)
			log := string(commands)
			if remote {
				if !strings.Contains(log, "ssh|") || !strings.Contains(log, "zfs|remote|snapshot") || strings.Contains(log, "zfs|remote|receive") {
					t.Fatalf("remote routing log:\n%s", log)
				}
			} else if strings.Contains(log, "ssh|") {
				t.Fatalf("local mode invoked SSH:\n%s", log)
			}
		})
	}
}

func TestIntegrationSendFailureAndReceiveResumeBothModes(t *testing.T) {
	requireIntegration(t)
	for _, remote := range []bool{false, true} {
		for _, failure := range []string{"send-fail", "receive-fail"} {
			mode := sourceMode(remote)
			t.Run(mode+"-"+failure, func(t *testing.T) {
				withInterval(t, 5*time.Minute)
				name := strings.ReplaceAll("failure-"+mode+"-"+failure, "-fail", "f")
				source, dest, mount := resetDatasets(t, name)
				health := newHealthEvents(t)
				cfg := integrationConfig(name, source, dest, remote, health.server.URL+"/check")
				writeData(t, mount, "payload", strings.Repeat("data", 1<<16))
				setFault(t, failure)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
					t.Fatal("faulted backup succeeded")
				}
				if got := health.snapshot(); len(got) != 2 || !strings.HasSuffix(got[0], "/start") || !strings.HasSuffix(got[1], "/fail") {
					t.Fatalf("failure health events: %#v", got)
				}
				if failure == "receive-fail" {
					token := realZFS(t, "get", "-H", "-o", "value", "receive_resume_token", dest)
					if token == "-" || token == "" {
						t.Fatal("failed resumable receive did not leave a token")
					}
				}
				clearFault(t)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
					t.Fatal(err)
				}
				assertProtected(t, cfg)
			})
		}
	}
}

func TestIntegrationGUIDMismatchAndUnownedDestinationBothModes(t *testing.T) {
	requireIntegration(t)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run(mode, func(t *testing.T) {
			name := "mismatch-" + mode
			source, dest, _ := resetDatasets(t, name)
			realZFS(t, "snapshot", source+"@mzb-"+name+"-old")
			realZFS(t, "create", dest)
			cfg := integrationConfig(name, source, dest, remote, "")
			before := len(matchingSnapshots(t, source, name))
			if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil || !strings.Contains(err.Error(), "not owned") {
				t.Fatalf("unowned destination error: %v", err)
			}
			if len(matchingSnapshots(t, source, name)) != before {
				t.Fatal("unowned destination caused a source mutation")
			}
			for prop, value := range ownershipValues(cfg) {
				realZFS(t, "set", prop+"="+value, dest)
			}
			realZFS(t, "snapshot", dest+"@mzb-"+name+"-old")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil || !strings.Contains(err.Error(), "no common snapshot") {
				t.Fatalf("GUID mismatch error: %v", err)
			}
		})
	}
}

func TestIntegrationRecursiveMirrorAndEncryption(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 0)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run("recursive-"+mode, func(t *testing.T) {
			name := "recursive-" + mode
			source, dest, mount := resetDatasets(t, name)
			realZFS(t, "create", "-o", "mountpoint="+filepath.Join(mount, "child"), source+"/child")
			cfg := integrationConfig(name, source, dest, remote, "")
			cfg.Recursive = true
			writeData(t, filepath.Join(mount, "child"), "one", "child")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			realZFS(t, "create", dest+"/destination-only")
			writeData(t, mount, "two", "next")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			if _, err := commandOutput(context.Background(), "/usr/sbin/zfs", "list", dest+"/destination-only"); err == nil {
				t.Fatal("recursive mirror retained destination-only child")
			}
		})

		t.Run("encrypted-"+mode, func(t *testing.T) {
			name := "encrypted-" + mode
			source, dest, _ := resetDatasets(t, name+"-parent")
			zfsMayFail("destroy", "-r", source)
			key := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(key, []byte("integration-secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
			mount := filepath.Join("/mnt/mzbsource", name)
			realZFS(t, "create", "-o", "mountpoint="+mount, "-o", "encryption=aes-256-gcm", "-o", "keyformat=passphrase", "-o", "keylocation=file://"+key, source)
			writeData(t, mount, "secret", "encrypted payload")
			realZFS(t, "unmount", source)
			realZFS(t, "unload-key", source)
			cfg := integrationConfig(name, source, dest, remote, "")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			if got := realZFS(t, "get", "-H", "-o", "value", "encryption", dest); got == "off" {
				t.Fatal("encrypted source was not received raw")
			}
			commands, _ := os.ReadFile(commandLog)
			if !strings.Contains(string(commands), "send -w") {
				t.Fatalf("raw send not observed:\n%s", commands)
			}
		})
	}
}

func TestIntegrationInterruptionResumesAndReportsFailure(t *testing.T) {
	requireIntegration(t)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run(mode, func(t *testing.T) {
			name := "interruption-" + mode
			source, dest, mount := resetDatasets(t, name)
			f, err := os.Create(filepath.Join(mount, "large"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(f, bytes.NewReader(make([]byte, 1<<20)), 1<<20); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			health := newHealthEvents(t)
			cfg := integrationConfig(name, source, dest, remote, health.server.URL+"/check")
			setFault(t, "receive-slow")
			if err := os.WriteFile(commandLog, nil, 0666); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(processLog, nil, 0666); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- execute(ctx, cfg, logger{io.Discard}) }()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				log, _ := os.ReadFile(commandLog)
				if strings.Contains(string(log), "|receive ") {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			cancel()
			if err := <-done; err == nil {
				t.Fatal("interrupted backup succeeded")
			}
			assertRecordedProcessesExited(t)
			if got := health.snapshot(); len(got) < 2 || !strings.HasSuffix(got[len(got)-1], "/fail") {
				t.Fatalf("interruption health events: %#v", got)
			}
			clearFault(t)
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			assertProtected(t, cfg)
		})
	}
}

func TestIntegrationHoldFailuresReconcileBothModes(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 5*time.Minute)
	for _, remote := range []bool{false, true} {
		for _, fault := range []string{"hold-source-fail", "hold-dest-fail"} {
			mode := sourceMode(remote)
			t.Run(mode+"-"+fault, func(t *testing.T) {
				name := strings.ReplaceAll("hold-"+mode+"-"+fault, "-fail", "f")
				source, dest, mount := resetDatasets(t, name)
				health := newHealthEvents(t)
				cfg := integrationConfig(name, source, dest, remote, health.server.URL+"/check")
				writeData(t, mount, "payload", "protected only after both holds")
				setFault(t, fault)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
					t.Fatal("hold failure reported success")
				}
				clearFault(t)
				_ = os.WriteFile(commandLog, nil, 0666)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
					t.Fatal(err)
				}
				assertProtected(t, cfg)
				commands, _ := os.ReadFile(commandLog)
				if strings.Contains(string(commands), "|send ") || strings.Contains(string(commands), "|receive ") {
					t.Fatalf("hold reconciliation retransmitted data:\n%s", commands)
				}
				if got := health.snapshot(); len(got) != 4 || !strings.HasSuffix(got[1], "/fail") || got[3] != "/check" {
					t.Fatalf("hold recovery health events: %#v", got)
				}
			})
		}
	}
}

func TestIntegrationMutationAndVerificationFailuresBothModes(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 5*time.Minute)
	for _, remote := range []bool{false, true} {
		for _, fault := range []string{"snapshot-fail", "guid-fail", "received-guid-mismatch"} {
			mode := sourceMode(remote)
			t.Run(mode+"-"+fault, func(t *testing.T) {
				name := strings.ReplaceAll("mutation-"+mode+"-"+fault, "-fail", "f")
				source, dest, mount := resetDatasets(t, name)
				health := newHealthEvents(t)
				cfg := integrationConfig(name, source, dest, remote, health.server.URL+"/check")
				writeData(t, mount, "payload", fault)
				setFault(t, fault)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
					t.Fatal("injected mutation failure reported success")
				}
				if fault == "snapshot-fail" && len(matchingSnapshots(t, source, name)) != 0 {
					t.Fatal("failed snapshot creation left an application snapshot")
				}
				clearFault(t)
				if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
					t.Fatal(err)
				}
				assertProtected(t, cfg)
				if got := health.snapshot(); len(got) != 4 || !strings.HasSuffix(got[1], "/fail") || got[3] != "/check" {
					t.Fatalf("mutation recovery health events: %#v", got)
				}
			})
		}
	}
}

func TestIntegrationForeignHoldDoesNotEndangerNewSnapshot(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 0)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run(mode, func(t *testing.T) {
			name := "foreign-hold-" + mode
			source, dest, mount := resetDatasets(t, name)
			cfg := integrationConfig(name, source, dest, remote, "")
			writeData(t, mount, "one", "first")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			old := assertProtected(t, cfg)
			time.Sleep(time.Second)
			realZFS(t, "hold", "external-test", dest+"@"+old.Name)
			writeData(t, mount, "two", "second")
			var logs bytes.Buffer
			if err := execute(context.Background(), cfg, logger{&logs}); err != nil {
				t.Fatal(err)
			}
			if len(matchingSnapshots(t, source, name)) != 1 || len(matchingSnapshots(t, dest, name)) != 2 {
				t.Fatal("foreign hold did not preserve only the obstructed destination snapshot")
			}
			if !strings.Contains(logs.String(), " WARN cleanup-dest:") {
				t.Fatalf("cleanup obstruction was not logged:\n%s", logs.String())
			}
			realZFS(t, "release", "external-test", dest+"@"+old.Name)
			writeData(t, mount, "three", "third")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			assertProtected(t, cfg)
		})
	}
}

func TestIntegrationDivergenceAndMissedIntervalsBothModes(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 0)
	for _, remote := range []bool{false, true} {
		mode := sourceMode(remote)
		t.Run(mode, func(t *testing.T) {
			name := "recovery-" + mode
			source, dest, mount := resetDatasets(t, name)
			cfg := integrationConfig(name, source, dest, remote, "")
			writeData(t, mount, "initial", "base")
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			realZFS(t, "snapshot", dest+"@manual-newer")
			before := len(matchingSnapshots(t, source, name))
			if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil || !strings.Contains(err.Error(), "newer or unexpected snapshot") {
				t.Fatalf("destination divergence was not rejected: %v", err)
			}
			if len(matchingSnapshots(t, source, name)) != before {
				t.Fatal("destination divergence mutated the source")
			}
			realZFS(t, "destroy", dest+"@manual-newer")
			for i := 0; i < 3; i++ {
				writeData(t, mount, "missed", strings.Repeat("x", i+1))
				realZFS(t, "snapshot", source+"@mzb-"+name+"-missed"+string(rune('a'+i)))
				time.Sleep(time.Second)
			}
			_ = os.WriteFile(commandLog, nil, 0666)
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			if got := assertProtected(t, cfg); got.Name != "mzb-"+name+"-missedc" {
				t.Fatalf("recovered snapshot %s, want newest missed snapshot", got.Name)
			}
			commands, _ := os.ReadFile(commandLog)
			if !strings.Contains(string(commands), "send -i ") {
				t.Fatalf("missed-interval recovery was not incremental:\n%s", commands)
			}
		})
	}
}

func TestIntegrationExecuteDiscoveryFailures(t *testing.T) {
	requireIntegration(t)
	for _, fault := range []string{"source-exists-fail", "encryption-fail", "source-snapshots-fail", "dest-exists-fail"} {
		t.Run(fault, func(t *testing.T) {
			name := strings.ReplaceAll("discovery-"+fault, "-fail", "f")
			source, dest, _ := resetDatasets(t, name)
			health := newHealthEvents(t)
			setFault(t, fault)
			if err := execute(context.Background(), integrationConfig(name, source, dest, false, health.server.URL+"/check"), logger{io.Discard}); err == nil {
				t.Fatal("discovery fault reported success")
			}
			if got := health.snapshot(); len(got) != 2 || !strings.HasSuffix(got[0], "/start") || !strings.HasSuffix(got[1], "/fail") {
				t.Fatalf("discovery failure health events: %#v", got)
			}
		})
	}

	t.Run("source-missing", func(t *testing.T) {
		name := "source-missing"
		source, dest, _ := resetDatasets(t, name)
		if err := execute(context.Background(), integrationConfig(name, source+"/missing", dest, false, ""), logger{io.Discard}); err == nil {
			t.Fatal("missing source reported success")
		}
	})

	t.Run("destination-parent-missing", func(t *testing.T) {
		name := "parent-missing"
		source, _, _ := resetDatasets(t, name)
		if err := execute(context.Background(), integrationConfig(name, source, "missingpool/backup", false, ""), logger{io.Discard}); err == nil {
			t.Fatal("missing destination parent reported success")
		}
	})

	t.Run("future-source-snapshot", func(t *testing.T) {
		name := "future-snapshot"
		source, dest, _ := resetDatasets(t, name)
		realZFS(t, "snapshot", source+"@mzb-"+name+"-future")
		setFault(t, "source-snapshot-future")
		if err := execute(context.Background(), integrationConfig(name, source, dest, false, ""), logger{io.Discard}); err == nil || !strings.Contains(err.Error(), "in the future") {
			t.Fatalf("future snapshot error: %v", err)
		}
	})
}

func TestIntegrationExecuteDestinationStateFailures(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 5*time.Minute)
	for _, fault := range []string{"destination-token-fail", "dest-snapshots-fail", "dest-root-snapshots-fail", "dest-root-snapshots-empty", "holds-source-fail", "holds-dest-fail"} {
		t.Run(fault, func(t *testing.T) {
			name := strings.ReplaceAll("state-"+fault, "-fail", "f")
			source, dest, mount := resetDatasets(t, name)
			cfg := integrationConfig(name, source, dest, false, "")
			writeData(t, mount, "payload", fault)
			if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
				t.Fatal(err)
			}
			before := assertProtected(t, cfg)
			setFault(t, fault)
			if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
				t.Fatal("destination-state fault reported success")
			}
			clearFault(t)
			if after := assertProtected(t, cfg); after.GUID != before.GUID {
				t.Fatal("destination-state failure advanced the protected snapshot")
			}
		})
	}

	t.Run("destination-properties", func(t *testing.T) {
		name := "destination-properties"
		source, dest, mount := resetDatasets(t, name)
		cfg := integrationConfig(name, source, dest, false, "")
		writeData(t, mount, "payload", "properties")
		setFault(t, "destination-set-fail")
		if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
			t.Fatal("destination property failure reported success")
		}
		clearFault(t)
		if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
			t.Fatal(err)
		}
		assertProtected(t, cfg)
	})
}

func TestIntegrationExecuteProgressAndResumeEdges(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 5*time.Minute)

	t.Run("full-estimate", func(t *testing.T) {
		name := "progress-full"
		source, dest, mount := resetDatasets(t, name)
		cfg := integrationConfig(name, source, dest, false, "")
		cfg.Progress = true
		writeData(t, mount, "payload", "estimate")
		if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
			t.Fatal(err)
		}
		commands, _ := os.ReadFile(commandLog)
		if !strings.Contains(string(commands), "send -nP") {
			t.Fatalf("send estimate not observed:\n%s", commands)
		}
	})

	t.Run("estimate-failure-falls-back", func(t *testing.T) {
		name := "progress-fallback"
		source, dest, mount := resetDatasets(t, name)
		cfg := integrationConfig(name, source, dest, false, "")
		cfg.Progress = true
		writeData(t, mount, "payload", "fallback")
		setFault(t, "send-estimate-fail")
		var logs bytes.Buffer
		if err := execute(context.Background(), cfg, logger{&logs}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), " WARN send-estimate:") {
			t.Fatalf("estimate failure was not logged:\n%s", logs.String())
		}
	})

	t.Run("resume-validation-and-estimate", func(t *testing.T) {
		name := "progress-resume"
		source, dest, mount := resetDatasets(t, name)
		cfg := integrationConfig(name, source, dest, false, "")
		writeData(t, mount, "payload", strings.Repeat("resume", 1<<16))
		setFault(t, "receive-fail")
		if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil {
			t.Fatal("receive fault reported success")
		}
		setFault(t, "resume-token-wrong")
		if err := execute(context.Background(), cfg, logger{io.Discard}); err == nil || !strings.Contains(err.Error(), "not this backup") {
			t.Fatalf("wrong resume token error: %v", err)
		}
		setFault(t, "resume-size-zero")
		cfg.Progress = true
		if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
			t.Fatal(err)
		}
		assertProtected(t, cfg)
	})
}

func TestIntegrationExecutePostCleanupAndHealthcheckEdges(t *testing.T) {
	requireIntegration(t)
	withInterval(t, 0)

	t.Run("canceled-during-cleanup", func(t *testing.T) {
		name := "cleanup-cancel"
		source, dest, mount := resetDatasets(t, name)
		cfg := integrationConfig(name, source, dest, false, "")
		writeData(t, mount, "one", "first")
		if err := execute(context.Background(), cfg, logger{io.Discard}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		writeData(t, mount, "two", "second")
		setFault(t, "cleanup-slow")
		_ = os.WriteFile(commandLog, nil, 0666)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- execute(ctx, cfg, logger{io.Discard}) }()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			log, _ := os.ReadFile(commandLog)
			if strings.Contains(string(log), "|release ") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cleanup cancellation error: %v", err)
		}
	})

	t.Run("healthcheck-non-2xx", func(t *testing.T) {
		name := "health-warning"
		source, dest, mount := resetDatasets(t, name)
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		writeData(t, mount, "payload", "health")
		var logs bytes.Buffer
		if err := execute(context.Background(), integrationConfig(name, source, dest, false, server.URL+"/check"), logger{&logs}); err != nil {
			t.Fatal(err)
		}
		if requests != 2 || !strings.Contains(logs.String(), " WARN healthcheck:") {
			t.Fatalf("healthcheck warnings: requests=%d logs=%q", requests, logs.String())
		}
	})
}

func TestIntegrationRemoteDestinationRejectedBeforeCommands(t *testing.T) {
	requireIntegration(t)
	binary := os.Getenv("MZB_BINARY")
	if binary == "" {
		t.Skip("MZB_BINARY is set by the VM harness")
	}
	_ = os.WriteFile(commandLog, nil, 0666)
	cmd := exec.Command(binary, "--name", "reject", "--source", integrationSourcePool+"/data", "--dest", "user@host:tank/data")
	if err := cmd.Run(); err == nil {
		t.Fatal("remote destination was accepted")
	}
	if log, _ := os.ReadFile(commandLog); len(log) != 0 {
		t.Fatalf("validation invoked external commands:\n%s", log)
	}
}
