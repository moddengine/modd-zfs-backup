package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	clockSkewLimit  = 30 * time.Second
	stderrLimit     = 64 << 10
	ownerNameProp   = "com.modd:backup-name"
	ownerSourceProp = "com.modd:backup-source"
	ownerRecurProp  = "com.modd:backup-recursive"
)

var minimumInterval = 5 * time.Minute

var (
	nameRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	datasetPartRE = regexp.MustCompile(`^[A-Za-z0-9_.:%-]+$`)
	sshSourceRE   = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)@([A-Za-z0-9][A-Za-z0-9._-]*):(.*)$`)
)

type Source struct {
	Remote  bool
	SSHHost string
	Dataset string
}

type Config struct {
	Name, Dest, HealthcheckURL string
	Source                     Source
	Recursive, Progress        bool
}

type ZFSRunner interface {
	Run(context.Context, ...string) error
	Output(context.Context, ...string) ([]byte, error)
}

type localZFS struct{}

func (localZFS) Run(ctx context.Context, args ...string) error {
	return runCommand(ctx, "zfs", args...)
}

func (localZFS) Output(ctx context.Context, args ...string) ([]byte, error) {
	return commandOutput(ctx, "zfs", args...)
}

type remoteZFS struct{ host string }

func (r remoteZFS) Run(ctx context.Context, args ...string) error {
	return runCommand(ctx, "ssh", remoteArgs(r.host, args...)...)
}

func (r remoteZFS) Output(ctx context.Context, args ...string) ([]byte, error) {
	return commandOutput(ctx, "ssh", remoteArgs(r.host, args...)...)
}

type snapshot struct {
	Name string
	GUID uint64
	When time.Time
}

type runMode string

const (
	modeFull        runMode = "full"
	modeIncremental runMode = "incremental"
	modeResume      runMode = "resume"
	modeRetry       runMode = "retry"
	modeReconcile   runMode = "reconcile"
)

type logger struct{ out io.Writer }

func (l logger) log(level, step, format string, args ...any) {
	message := strings.ReplaceAll(fmt.Sprintf(format, args...), "\r", "")
	for _, line := range strings.Split(message, "\n") {
		fmt.Fprintf(l.out, "%s %s %s: %s\n", time.Now().Format(time.RFC3339), level, step, line)
	}
}
func (l logger) info(step, format string, args ...any) { l.log("INFO", step, format, args...) }
func (l logger) warn(step, format string, args ...any) { l.log("WARN", step, format, args...) }
func (l logger) err(step, format string, args ...any)  { l.log("ERROR", step, format, args...) }

func main() {
	var rawSource string
	var cfg Config
	flag.StringVar(&cfg.Name, "name", "", "backup name")
	flag.StringVar(&rawSource, "source", "", "local dataset or user@host:dataset")
	flag.StringVar(&cfg.Dest, "dest", "", "local destination dataset")
	flag.StringVar(&cfg.HealthcheckURL, "healthcheck-url", "", "Healthchecks.io ping URL")
	flag.BoolVar(&cfg.Recursive, "recursive", false, "mirror descendant datasets")
	flag.BoolVar(&cfg.Progress, "progress", false, "show interactive transfer progress")
	flag.Parse()

	l := logger{os.Stderr}
	l.info("startup", "modd-zfs-backup starting")
	source, err := parseSource(rawSource)
	if err == nil {
		cfg.Source = source
		err = validateConfig(cfg)
	}
	if err != nil {
		l.err("config", "%v", err)
		os.Exit(2)
	}
	l.info("config", "backup name=%s", cfg.Name)
	l.info("config", "source=%s", formatSource(cfg.Source))
	l.info("config", "destination=%s", cfg.Dest)
	l.info("config", "recursive=%t", cfg.Recursive)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, cfg, l); err != nil {
		os.Exit(1)
	}
}

func parseSource(s string) (Source, error) {
	if s == "" {
		return Source{}, errors.New("--source is required")
	}
	if match := sshSourceRE.FindStringSubmatch(s); match != nil {
		if err := validateDataset(match[3]); err != nil {
			return Source{}, fmt.Errorf("invalid source dataset: %w", err)
		}
		return Source{Remote: true, SSHHost: match[1] + "@" + match[2], Dataset: match[3]}, nil
	}
	if strings.Contains(s, "@") {
		return Source{}, fmt.Errorf("invalid remote source %q (expected user@host:dataset)", s)
	}
	if err := validateDataset(s); err != nil {
		return Source{}, fmt.Errorf("invalid source dataset: %w", err)
	}
	return Source{Dataset: s}, nil
}

func validateDataset(s string) error {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, "@# 	\r\n") {
		return fmt.Errorf("invalid dataset %q", s)
	}
	parts := strings.Split(s, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0][0] < 'A' || (parts[0][0] > 'Z' && parts[0][0] < 'a') || parts[0][0] > 'z' {
		return fmt.Errorf("invalid dataset %q", s)
	}
	for _, part := range parts {
		if !datasetPartRE.MatchString(part) || part == "." || part == ".." {
			return fmt.Errorf("invalid dataset %q", s)
		}
	}
	return nil
}

func validateConfig(c Config) error {
	if c.Name == "" {
		return errors.New("--name is required")
	}
	if !nameRE.MatchString(c.Name) {
		return errors.New("--name may contain only letters, numbers, '.', '_' and '-'")
	}
	if c.Source.Dataset == "" {
		return errors.New("--source is required")
	}
	if c.Dest == "" {
		return errors.New("--dest is required")
	}
	if sshSourceRE.MatchString(c.Dest) {
		return errors.New("--dest must be a local ZFS dataset; remote destinations are unsupported")
	}
	if err := validateDataset(c.Dest); err != nil {
		return fmt.Errorf("invalid destination dataset: %w", err)
	}
	if len(c.Source.Dataset+"@mzb-"+c.Name+"-"+strings.Repeat("9", 20)) > 255 || len(c.Dest+"@mzb-"+c.Name+"-"+strings.Repeat("9", 20)) > 255 {
		return errors.New("--name is too long for the source or destination snapshot path")
	}
	if !c.Source.Remote && c.Source.Dataset == c.Dest {
		return errors.New("source and destination must differ")
	}
	if !c.Source.Remote && c.Recursive && strings.HasPrefix(c.Dest, c.Source.Dataset+"/") {
		return errors.New("recursive destination must not be beneath the source dataset")
	}
	if c.HealthcheckURL != "" {
		u, err := url.ParseRequestURI(c.HealthcheckURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return errors.New("--healthcheck-url must be an HTTP or HTTPS URL without credentials")
		}
	}
	return nil
}

func formatSource(s Source) string {
	if s.Remote {
		return s.SSHHost + ":" + s.Dataset
	}
	return s.Dataset
}

func execute(ctx context.Context, cfg Config, l logger) error {
	started := time.Now()
	lock, acquired, err := acquireLock(cfg.Name)
	if err != nil {
		return finishFailure(cfg, l, started, "lock", "", err)
	}
	if !acquired {
		l.info("skip", "backup %s is already running", cfg.Name)
		return nil
	}
	defer lock.Close()
	l.info("lock", "acquired backup lock")

	sourceRunner := ZFSRunner(localZFS{})
	if cfg.Source.Remote {
		sourceRunner = remoteZFS{cfg.Source.SSHHost}
		l.info("source", "connecting to %s", cfg.Source.SSHHost)
	}
	if err := requireDataset(ctx, sourceRunner, cfg.Source.Dataset); err != nil {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "source", "", err, true)
	}
	l.info("source", "dataset %s found", cfg.Source.Dataset)
	raw, err := encryptedSource(ctx, sourceRunner, cfg.Source.Dataset)
	if err != nil {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "source", "", fmt.Errorf("check encryption: %w", err), true)
	}
	if raw {
		l.info("source", "encrypted source detected; using raw sends")
	}

	l.info("snapshots-source", "discovering source snapshots")
	sourceSnaps, err := listSnapshots(ctx, sourceRunner, cfg.Source.Dataset, cfg.Name)
	if err != nil {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "snapshots-source", "", err, true)
	}
	l.info("snapshots-source", "found %d matching source snapshots", len(sourceSnaps))
	if len(sourceSnaps) > 0 && sourceSnaps[len(sourceSnaps)-1].When.After(time.Now().Add(clockSkewLimit)) {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "interval", sourceSnaps[len(sourceSnaps)-1].Name, errors.New("newest source snapshot is in the future; check source and backup host clocks"), true)
	}

	destRunner := localZFS{}
	destExists, err := datasetExists(ctx, destRunner, cfg.Dest)
	if err != nil {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "destination", "", err, true)
	}
	if !destExists {
		if err := requireDestinationParent(ctx, destRunner, cfg.Dest); err != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "destination", "", err, true)
		}
		l.info("destination", "dataset %s will be created by receive", cfg.Dest)
	} else {
		l.info("destination", "dataset %s found", cfg.Dest)
	}

	var destSnaps []snapshot
	resumeToken := ""
	if destExists {
		resumeToken, err = destinationToken(ctx, destRunner, cfg.Dest)
		if err != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "destination", "", err, true)
		}
		if err := verifyOwnership(ctx, destRunner, cfg); err != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "destination", "", err, true)
		}
		destSnaps, err = listSnapshots(ctx, destRunner, cfg.Dest, cfg.Name)
		if err != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "snapshots-dest", "", err, true)
		}
	}
	l.info("snapshots-dest", "found %d matching destination snapshots", len(destSnaps))

	base := commonSnapshot(sourceSnaps, destSnaps)
	if destExists && base != nil && !cfg.Recursive {
		allDestSnaps, listErr := listRootSnapshots(ctx, destRunner, cfg.Dest)
		if listErr != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "snapshots-dest", "", listErr, true)
		}
		if len(allDestSnaps) == 0 || allDestSnaps[len(allDestSnaps)-1].GUID != base.GUID {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "common", base.Name, errors.New("destination has a newer or unexpected snapshot; refusing incremental receive"), true)
		}
	}
	var target *snapshot
	mode := modeFull
	total := int64(0)
	if resumeToken != "" {
		target, total, err = validateResumeToken(ctx, sourceRunner, cfg, sourceSnaps, resumeToken)
		if err != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "resume", "", err, true)
		}
		mode = modeResume
	} else if destExists && base == nil {
		startHealthcheck(ctx, cfg, l)
		return failRun(ctx, cfg, l, started, "common", "", errors.New("existing destination has no common snapshot; refusing destructive full replacement"), true)
	} else if len(sourceSnaps) > 0 && (base == nil || sourceSnaps[len(sourceSnaps)-1].GUID != base.GUID) {
		target = &sourceSnaps[len(sourceSnaps)-1]
		if base == nil {
			mode = modeFull
		} else {
			mode = modeRetry
		}
	} else if base != nil {
		target = base
		sourceHeld, sourceHoldErr := hasHold(ctx, sourceRunner, cfg.Source.Dataset+"@"+target.Name, "mzb-"+cfg.Name)
		destHeld, destHoldErr := hasHold(ctx, destRunner, cfg.Dest+"@"+target.Name, "mzb-"+cfg.Name)
		if sourceHoldErr != nil || destHoldErr != nil {
			startHealthcheck(ctx, cfg, l)
			return failRun(ctx, cfg, l, started, "common", target.Name, errors.Join(sourceHoldErr, destHoldErr), true)
		}
		age := time.Since(target.When)
		if (!sourceHeld || !destHeld) && age < minimumInterval {
			mode = modeReconcile
		} else if sourceHeld && destHeld && age < minimumInterval {
			l.info("skip", "newest protected snapshot is %s old; minimum interval is %s", age.Round(time.Second), minimumInterval)
			return nil
		} else {
			target = nil
			mode = modeIncremental
		}
	}

	startHealthcheck(ctx, cfg, l)
	if target == nil {
		created := snapshot{Name: fmt.Sprintf("mzb-%s-%d", cfg.Name, time.Now().UnixNano()), When: time.Now()}
		path := cfg.Source.Dataset + "@" + created.Name
		l.info("snapshot-create", "creating source snapshot %s", path)
		args := []string{"snapshot"}
		if cfg.Recursive {
			args = append(args, "-r")
		}
		if err := sourceRunner.Run(ctx, append(args, path)...); err != nil {
			return failRun(ctx, cfg, l, started, "snapshot-create", created.Name, err, true)
		}
		guid, err := snapshotGUID(ctx, sourceRunner, path)
		if err != nil {
			return failRun(ctx, cfg, l, started, "snapshot-create", created.Name, err, true)
		}
		created.GUID = guid
		target = &created
		l.info("snapshot-create", "source snapshot created")
	}

	bytesSent := int64(0)
	transferDuration := time.Duration(0)
	if mode != modeReconcile {
		if cfg.Progress && total == 0 {
			l.info("send-estimate", "estimating replication stream size")
			if mode == modeResume {
				total, err = estimateResume(ctx, sourceRunner, resumeToken)
			} else {
				total, err = estimateSend(ctx, sourceRunner, cfg, base, target.Name, raw)
			}
			if err != nil {
				l.warn("send-estimate", "unable to determine expected send size; showing transferred bytes only: %v", err)
			}
		}
		l.info("send", "starting %s replication snapshot=%s", mode, target.Name)
		bytesSent, transferDuration, err = replicate(ctx, cfg, base, target.Name, resumeToken, total, raw, l)
		if err != nil {
			var pe *pipelineError
			step := "send"
			if errors.As(err, &pe) {
				step = pe.Primary
			}
			return failRun(ctx, cfg, l, started, step, target.Name, err, true)
		}
		l.info("send", "replication complete bytes=%s duration=%s average=%s/s", formatBytes(bytesSent), transferDuration.Round(time.Second), formatBytes(rate(bytesSent, transferDuration)))
		if err := verifyReceivedSnapshot(ctx, destRunner, cfg.Dest, *target); err != nil {
			return failRun(ctx, cfg, l, started, "receive", target.Name, err, true)
		}
		if err := setDestinationProperties(ctx, destRunner, cfg); err != nil {
			return failRun(ctx, cfg, l, started, "destination", target.Name, err, true)
		}
	}

	tag := "mzb-" + cfg.Name
	if err := ensureHold(ctx, sourceRunner, tag, cfg.Source.Dataset+"@"+target.Name, cfg.Recursive, "hold-source", l); err != nil {
		return failRun(ctx, cfg, l, started, "hold-source", target.Name, err, true)
	}
	if err := ensureHold(ctx, destRunner, tag, cfg.Dest+"@"+target.Name, cfg.Recursive, "hold-dest", l); err != nil {
		return failRun(ctx, cfg, l, started, "hold-dest", target.Name, err, true)
	}
	cleanup(ctx, sourceRunner, tag, cfg.Source.Dataset, sourceSnaps, target.Name, cfg.Recursive, "cleanup-source", l)
	cleanup(ctx, destRunner, tag, cfg.Dest, destSnaps, target.Name, cfg.Recursive, "cleanup-dest", l)
	if err := ctx.Err(); err != nil {
		return failRun(ctx, cfg, l, started, "cleanup", target.Name, err, true)
	}
	if err := pingHealthcheck(ctx, cfg.HealthcheckURL, "", l); err != nil {
		l.warn("healthcheck", "success notification failed: %v", err)
	}
	l.info("complete", "backup %s completed snapshot=%s mode=%s bytes=%s duration=%s", cfg.Name, target.Name, mode, formatBytes(bytesSent), time.Since(started).Round(time.Second))
	return nil
}

func lockDirectory() string {
	if dir := os.Getenv("MODD_ZFS_BACKUP_LOCK_DIR"); dir != "" {
		return dir
	}
	return "/run/lock/modd-zfs-backup"
}

func acquireLock(name string) (*os.File, bool, error) {
	dir := lockDirectory()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return f, true, nil
}

func requireDataset(ctx context.Context, runner ZFSRunner, dataset string) error {
	ok, err := datasetExists(ctx, runner, dataset)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("dataset %s does not exist", dataset)
	}
	return nil
}

func requireDestinationParent(ctx context.Context, runner ZFSRunner, dest string) error {
	i := strings.LastIndexByte(dest, '/')
	if i < 0 {
		return fmt.Errorf("destination pool %s does not exist", dest)
	}
	return requireDataset(ctx, runner, dest[:i])
}

func datasetExists(ctx context.Context, runner ZFSRunner, dataset string) (bool, error) {
	_, err := runner.Output(ctx, "list", "-H", "-o", "name", dataset)
	if err == nil {
		return true, nil
	}
	var ce *commandError
	if errors.As(err, &ce) && ce.ExitCode == 1 && strings.Contains(strings.ToLower(ce.Stderr), "does not exist") {
		return false, nil
	}
	return false, err
}

func encryptedSource(ctx context.Context, runner ZFSRunner, dataset string) (bool, error) {
	out, err := runner.Output(ctx, "get", "-H", "-o", "value", "-r", "encryptionroot", dataset)
	if err != nil {
		return false, err
	}
	for _, value := range strings.Fields(string(out)) {
		if value != "-" {
			return true, nil
		}
	}
	return false, nil
}

func listSnapshots(ctx context.Context, runner ZFSRunner, dataset, name string) ([]snapshot, error) {
	out, err := runner.Output(ctx, "list", "-H", "-p", "-t", "snapshot", "-o", "name,guid,creation", "-s", "creation", "-r", dataset)
	if err != nil {
		return nil, err
	}
	prefix := dataset + "@mzb-" + name + "-"
	var result []snapshot
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		guid, guidErr := strconv.ParseUint(fields[1], 10, 64)
		created, timeErr := strconv.ParseInt(fields[2], 10, 64)
		if guidErr != nil || timeErr != nil {
			return nil, fmt.Errorf("invalid zfs snapshot listing %q", line)
		}
		result = append(result, snapshot{Name: strings.TrimPrefix(fields[0], dataset+"@"), GUID: guid, When: time.Unix(created, 0)})
	}
	return result, nil
}

func listRootSnapshots(ctx context.Context, runner ZFSRunner, dataset string) ([]snapshot, error) {
	out, err := runner.Output(ctx, "list", "-H", "-p", "-t", "snapshot", "-o", "name,guid,creation", "-s", "creation", "-r", dataset)
	if err != nil {
		return nil, err
	}
	var result []snapshot
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.HasPrefix(fields[0], dataset+"@") {
			continue
		}
		guid, guidErr := strconv.ParseUint(fields[1], 10, 64)
		created, timeErr := strconv.ParseInt(fields[2], 10, 64)
		if guidErr != nil || timeErr != nil {
			return nil, fmt.Errorf("invalid zfs snapshot listing %q", line)
		}
		result = append(result, snapshot{Name: strings.TrimPrefix(fields[0], dataset+"@"), GUID: guid, When: time.Unix(created, 0)})
	}
	return result, nil
}

func commonSnapshot(source, dest []snapshot) *snapshot {
	byGUID := make(map[uint64]bool, len(dest))
	for _, s := range dest {
		byGUID[s.GUID] = true
	}
	for i := len(source) - 1; i >= 0; i-- {
		if byGUID[source[i].GUID] {
			return &source[i]
		}
	}
	return nil
}

func snapshotGUID(ctx context.Context, runner ZFSRunner, path string) (uint64, error) {
	out, err := runner.Output(ctx, "get", "-H", "-p", "-o", "value", "guid", path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

func destinationToken(ctx context.Context, runner ZFSRunner, dest string) (string, error) {
	out, err := runner.Output(ctx, "get", "-H", "-o", "value", "receive_resume_token", dest)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "-" {
		return "", nil
	}
	return token, nil
}

func ownershipValues(cfg Config) map[string]string {
	return map[string]string{ownerNameProp: cfg.Name, ownerSourceProp: formatSource(cfg.Source), ownerRecurProp: strconv.FormatBool(cfg.Recursive)}
}

func verifyOwnership(ctx context.Context, runner ZFSRunner, cfg Config) error {
	for prop, want := range ownershipValues(cfg) {
		out, err := runner.Output(ctx, "get", "-H", "-o", "value", prop, cfg.Dest)
		if err != nil {
			return fmt.Errorf("destination ownership property %s: %w", prop, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			return fmt.Errorf("destination is not owned by this backup: %s=%q, want %q", prop, got, want)
		}
	}
	return nil
}

func setDestinationProperties(ctx context.Context, runner ZFSRunner, cfg Config) error {
	args := []string{"set", "readonly=on", "canmount=off", "mountpoint=none"}
	for prop, value := range ownershipValues(cfg) {
		args = append(args, prop+"="+value)
	}
	return runner.Run(ctx, append(args, cfg.Dest)...)
}

func receiveArgs(cfg Config) []string {
	args := []string{"receive", "-s", "-u"}
	if cfg.Recursive {
		args = append(args, "-F")
	}
	args = append(args, "-o", "readonly=on", "-o", "canmount=off", "-o", "mountpoint=none")
	for prop, value := range ownershipValues(cfg) {
		args = append(args, "-o", prop+"="+value)
	}
	return append(args, cfg.Dest)
}

func sendArgs(cfg Config, base *snapshot, snapName string, estimate, raw bool) []string {
	args := []string{"send"}
	if estimate {
		args = append(args, "-nP")
	}
	if raw {
		args = append(args, "-w")
	}
	if cfg.Recursive {
		args = append(args, "-R")
	}
	if base != nil {
		args = append(args, "-i", cfg.Source.Dataset+"@"+base.Name)
	}
	return append(args, cfg.Source.Dataset+"@"+snapName)
}

func estimateSend(ctx context.Context, runner ZFSRunner, cfg Config, base *snapshot, snapName string, raw bool) (int64, error) {
	out, err := runner.Output(ctx, sendArgs(cfg, base, snapName, true, raw)...)
	if err != nil {
		return 0, err
	}
	return parseSendField(out, "size")
}

func estimateResume(ctx context.Context, runner ZFSRunner, token string) (int64, error) {
	out, err := runner.Output(ctx, "send", "-nP", "-t", token)
	if err != nil {
		return 0, err
	}
	return parseSendField(out, "size")
}

func parseSendField(out []byte, key string) (int64, error) {
	value := sendField(out, key)
	if value != "" {
		return strconv.ParseInt(value, 10, 64)
	}
	return 0, fmt.Errorf("zfs send output did not contain %s", key)
}

func sendField(out []byte, key string) string {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			if fields[1] == "=" && len(fields) >= 3 {
				return fields[2]
			}
			return fields[1]
		}
	}
	return ""
}

func validateResumeToken(ctx context.Context, runner ZFSRunner, cfg Config, snaps []snapshot, token string) (*snapshot, int64, error) {
	out, err := runner.Output(ctx, "send", "-nP", "-t", token)
	if err != nil {
		return nil, 0, err
	}
	toName := sendField(out, "toname")
	total, _ := strconv.ParseInt(sendField(out, "size"), 10, 64)
	prefix := cfg.Source.Dataset + "@mzb-" + cfg.Name + "-"
	if !strings.HasPrefix(toName, prefix) {
		return nil, 0, fmt.Errorf("resume token targets %q, not this backup", toName)
	}
	short := strings.TrimPrefix(toName, cfg.Source.Dataset+"@")
	for i := range snaps {
		if snaps[i].Name == short {
			return &snaps[i], total, nil
		}
	}
	return nil, 0, fmt.Errorf("resume target %s no longer exists on source", toName)
}

func sourceCommand(ctx context.Context, source Source, args ...string) *exec.Cmd {
	if source.Remote {
		return newCommand(ctx, "ssh", remoteArgs(source.SSHHost, args...)...)
	}
	return newCommand(ctx, "zfs", args...)
}

func remoteArgs(host string, args ...string) []string {
	ssh := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", host, "zfs"}
	return append(ssh, args...)
}

type pipelineError struct {
	Primary       string
	Send, Receive error
	Copy          error
}

func (e *pipelineError) Error() string {
	parts := []string{"replication pipeline failed"}
	if e.Send != nil {
		parts = append(parts, "send: "+e.Send.Error())
	}
	if e.Receive != nil {
		parts = append(parts, "receive: "+e.Receive.Error())
	}
	if e.Copy != nil {
		parts = append(parts, "stream: "+e.Copy.Error())
	}
	return strings.Join(parts, "\n")
}

func replicate(ctx context.Context, cfg Config, base *snapshot, snapName, resumeToken string, total int64, raw bool, l logger) (int64, time.Duration, error) {
	started := time.Now()
	args := sendArgs(cfg, base, snapName, false, raw)
	if resumeToken != "" {
		args = []string{"send", "-t", resumeToken}
	}
	send := sourceCommand(ctx, cfg.Source, args...)
	recv := newCommand(ctx, "zfs", receiveArgs(cfg)...)
	sendErr, recvErr := &limitedBuffer{limit: stderrLimit}, &limitedBuffer{limit: stderrLimit}
	send.Stderr, recv.Stderr = sendErr, recvErr
	stream, err := send.StdoutPipe()
	if err != nil {
		return 0, 0, &pipelineError{Primary: "send", Send: err}
	}
	recvIn, err := recv.StdinPipe()
	if err != nil {
		return 0, 0, &pipelineError{Primary: "receive", Receive: err}
	}
	if err := recv.Start(); err != nil {
		return 0, 0, &pipelineError{Primary: "receive", Receive: commandFailure("local zfs receive", err, recvErr)}
	}
	if err := send.Start(); err != nil {
		_ = recvIn.Close()
		_ = recv.Wait()
		return 0, 0, &pipelineError{Primary: "send", Send: commandFailure(commandType(cfg.Source)+" zfs send", err, sendErr)}
	}
	progress := newProgress(stream, total, cfg.Progress && stderrIsTerminal(), snapName, l.out)
	_, copyErr := io.Copy(recvIn, progress)
	_ = stream.Close()
	_ = recvIn.Close()
	sendWait, recvWait := send.Wait(), recv.Wait()
	progress.finish()
	var sendFailure, recvFailure error
	if sendWait != nil {
		sendFailure = commandFailure(commandType(cfg.Source)+" zfs send", sendWait, sendErr)
	}
	if recvWait != nil {
		recvFailure = commandFailure("local zfs receive", recvWait, recvErr)
	}
	if sendFailure != nil || recvFailure != nil || copyErr != nil {
		primary := "send"
		if recvFailure != nil && copyErr != nil {
			primary = "receive"
		} else if sendFailure == nil && recvFailure != nil {
			primary = "receive"
		}
		return progress.read, time.Since(started), &pipelineError{Primary: primary, Send: sendFailure, Receive: recvFailure, Copy: copyErr}
	}
	return progress.read, time.Since(started), nil
}

func commandType(s Source) string {
	if s.Remote {
		return "remote"
	}
	return "local"
}

type progressReader struct {
	reader        io.Reader
	total, read   int64
	enabled       bool
	name          string
	out           io.Writer
	started, last time.Time
}

func newProgress(r io.Reader, total int64, enabled bool, name string, out io.Writer) *progressReader {
	now := time.Now()
	p := &progressReader{reader: r, total: total, enabled: enabled, name: name, out: out, started: now, last: now}
	if enabled {
		fmt.Fprintf(out, "Replicating %s\n", name)
	}
	return p
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	p.read += int64(n)
	if p.enabled && time.Since(p.last) >= time.Second {
		speed := rate(p.read, time.Since(p.started))
		if p.total > 0 {
			percent := float64(p.read) * 100 / float64(p.total)
			eta := time.Duration(0)
			if speed > 0 && p.total > p.read {
				eta = time.Duration(float64(p.total-p.read) / float64(speed) * float64(time.Second))
			}
			fmt.Fprintf(p.out, "\r%s / %s  %.1f%%  %s/s  ETA %s", formatBytes(p.read), formatBytes(p.total), percent, formatBytes(speed), eta.Round(time.Second))
		} else {
			fmt.Fprintf(p.out, "\r%s transferred  %s/s", formatBytes(p.read), formatBytes(speed))
		}
		p.last = time.Now()
	}
	return n, err
}

func (p *progressReader) finish() {
	if p.enabled {
		fmt.Fprintln(p.out)
	}
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func hasHold(ctx context.Context, runner ZFSRunner, snap, tag string) (bool, error) {
	out, err := runner.Output(ctx, "holds", "-H", snap)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == tag {
			return true, nil
		}
	}
	return false, nil
}

func ensureHold(ctx context.Context, runner ZFSRunner, tag, snap string, recursive bool, step string, l logger) error {
	held, err := hasHold(ctx, runner, snap, tag)
	if err != nil {
		return err
	}
	if held {
		l.info(step, "hold %s already present on %s", tag, snap)
		return nil
	}
	l.info(step, "applying hold %s to %s", tag, snap)
	args := []string{"hold"}
	if recursive {
		args = append(args, "-r")
	}
	if err := runner.Run(ctx, append(args, tag, snap)...); err != nil {
		return err
	}
	l.info(step, "hold applied")
	return nil
}

func cleanup(ctx context.Context, runner ZFSRunner, tag, dataset string, snaps []snapshot, keep string, recursive bool, step string, l logger) {
	for _, snap := range snaps {
		if snap.Name == keep || ctx.Err() != nil {
			continue
		}
		path := dataset + "@" + snap.Name
		args := []string{"release"}
		if recursive {
			args = append(args, "-r")
		}
		l.info(step, "releasing old snapshot %s", snap.Name)
		if err := runner.Run(ctx, append(args, tag, path)...); err != nil {
			l.warn(step, "unable to release old snapshot %s: %v", snap.Name, err)
		}
		args = []string{"destroy"}
		if recursive {
			args = append(args, "-r")
		}
		l.info(step, "destroying old snapshot %s", snap.Name)
		if err := runner.Run(ctx, append(args, path)...); err != nil {
			l.warn(step, "unable to destroy old snapshot %s (another hold may exist): %v", snap.Name, err)
		}
	}
}

func verifyReceivedSnapshot(ctx context.Context, runner ZFSRunner, dataset string, want snapshot) error {
	got, err := snapshotGUID(ctx, runner, dataset+"@"+want.Name)
	if err != nil {
		return err
	}
	if got != want.GUID {
		return fmt.Errorf("received snapshot GUID mismatch: got %d, want %d", got, want.GUID)
	}
	return nil
}

type commandError struct {
	Program, Stderr string
	ExitCode        int
	Truncated       bool
	Err             error
}

func (e *commandError) Error() string {
	message := fmt.Sprintf("%s failed with exit status %d: %v", e.Program, e.ExitCode, e.Err)
	if e.Stderr != "" {
		message += "\nstderr: " + e.Stderr
	}
	if e.Truncated {
		message += "\nstderr: [truncated at 64 KiB]"
	}
	return message
}
func (e *commandError) Unwrap() error { return e.Err }

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

func runCommand(ctx context.Context, program string, args ...string) error {
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd := newCommand(ctx, program, args...)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return commandFailure(program, err, stderr)
	}
	return nil
}

func commandOutput(ctx context.Context, program string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	stderr := &limitedBuffer{limit: stderrLimit}
	cmd := newCommand(ctx, program, args...)
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, commandFailure(program, err, stderr)
	}
	return stdout.Bytes(), nil
}

func newCommand(ctx context.Context, program string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func commandFailure(program string, err error, stderr *limitedBuffer) error {
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return &commandError{Program: program, ExitCode: exitCode, Stderr: strings.TrimSpace(stderr.String()), Truncated: stderr.truncated, Err: err}
}

func pingHealthcheck(ctx context.Context, base, suffix string, l logger) error {
	if base == "" {
		return nil
	}
	kind := "success"
	if suffix != "" {
		kind = suffix
	}
	l.info("healthcheck", "sending %s notification", kind)
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	if suffix != "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + suffix
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %s", resp.Status)
	}
	return nil
}

func startHealthcheck(ctx context.Context, cfg Config, l logger) {
	if err := pingHealthcheck(ctx, cfg.HealthcheckURL, "start", l); err != nil {
		l.warn("healthcheck", "start notification failed: %v", err)
	}
}

func failRun(ctx context.Context, cfg Config, l logger, started time.Time, step, snap string, err error, attempted bool) error {
	l.err(step, "%v", err)
	if attempted {
		pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if pingErr := pingHealthcheck(pingCtx, cfg.HealthcheckURL, "fail", l); pingErr != nil {
			l.warn("healthcheck", "failure notification failed: %v", pingErr)
		}
	}
	return finishFailure(cfg, l, started, step, snap, err)
}

func finishFailure(cfg Config, l logger, started time.Time, step, snap string, err error) error {
	l.err("complete", "backup %s failed step=%s snapshot=%s duration=%s", cfg.Name, step, snap, time.Since(started).Round(time.Second))
	return err
}

func rate(bytes int64, duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64(float64(bytes) / duration.Seconds())
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	for _, unit := range units {
		v /= 1024
		if v < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f%s", v, unit)
		}
	}
	return fmt.Sprintf("%dB", n)
}
