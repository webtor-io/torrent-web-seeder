package services

import (
	"os"
	"os/exec"
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type Snapshot struct {
	resticPassword     string
	resticRepository   string
	awsAccessKeyID     string
	awsSecretAccessKey string
	dataDir            string
	resticBin          string
	infoHash           string
	inited             bool
	state              SnapshotState
	err                error
	mi                 *MetaInfo
	mux                sync.Mutex
}

type SnapshotState int

const (
	SNAPSHOT_IDLE    SnapshotState = 0
	SNAPSHOT_RESTORE SnapshotState = 1
	SNAPSHOT_BACKUP  SnapshotState = 2
)

const (
	USE_SNAPSHOT          = "use-snapshot"
	RESTIC_PASSWORD       = "restic-pasword"
	RESTIC_REPOSITORY     = "restic-repository"
	AWS_ACCESS_KEY_ID     = "aws-access-key-id"
	AWS_SECRET_ACCESS_KEY = "aws-secret-access-key"
)

func RegisterSnapshotFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   USE_SNAPSHOT,
		Usage:  "use snapshot",
		EnvVar: "USE_SNAPSHOT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   RESTIC_PASSWORD,
		Usage:  "restic password",
		Value:  "",
		EnvVar: "RESTIC_PASSWORD",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   RESTIC_REPOSITORY,
		Usage:  "restic repository",
		Value:  "",
		EnvVar: "RESTIC_REPOSITORY",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_ACCESS_KEY_ID,
		Usage:  "AWS Access Key ID",
		Value:  "",
		EnvVar: "AWS_ACCESS_KEY_ID",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_SECRET_ACCESS_KEY,
		Usage:  "AWS Secret Access Key",
		Value:  "",
		EnvVar: "AWS_SECRET_ACCESS_KEY",
	})
}

func NewSnapshot(c *cli.Context, mi *MetaInfo) (*Snapshot, error) {
	if !c.Bool(USE_SNAPSHOT) {
		return nil, nil
	}
	if c.String(RESTIC_PASSWORD) == "" {
		return nil, errors.Errorf("Restic password can't be empty")
	}
	if c.String(RESTIC_REPOSITORY) == "" {
		return nil, errors.Errorf("Restic repository can't be empty")
	}
	if c.String(AWS_ACCESS_KEY_ID) == "" {
		return nil, errors.Errorf("AWS Access Key ID can't be empty")
	}
	if c.String(AWS_SECRET_ACCESS_KEY) == "" {
		return nil, errors.Errorf("AWS Secret Access Key can't be empty")
	}
	if mi == nil {
		return nil, errors.Errorf("MetaInfo must be provided")
	}
	return &Snapshot{resticPassword: c.String(RESTIC_PASSWORD), resticRepository: c.String(RESTIC_REPOSITORY),
		awsAccessKeyID: c.String(AWS_ACCESS_KEY_ID), awsSecretAccessKey: c.String(AWS_SECRET_ACCESS_KEY),
		dataDir: c.String(TORRENT_CLIENT_DATA_DIR_FLAG), inited: false, state: SNAPSHOT_IDLE, mi: mi}, nil
}

func (s *Snapshot) State() SnapshotState {
	return s.state
}

func (s *Snapshot) init() error {
	if s.inited {
		return s.err
	}
	defer func() { s.inited = true }()
	path, err := exec.LookPath("restic")
	if err != nil {
		s.err = errors.Wrap(err, "Failed to find restic")
		return s.err
	}
	s.resticBin = path
	mi, err := s.mi.Get()
	if err != nil {
		s.err = errors.Wrap(err, "Failed to get MetaInfo")
		return s.err
	}
	s.infoHash = mi.HashInfoBytes().HexString()
	cmd := exec.Command("mkdir", "-p", s.dataDir)
	cmd.Run()
	if err != nil {
		s.err = errors.Wrap(err, "Failed to create data dir")
		return s.err
	}
	s.err = nil
	return s.err
}

func (s *Snapshot) Restore() error {
	s.mux.Lock()
	s.state = SNAPSHOT_RESTORE
	defer func() {
		s.state = SNAPSHOT_IDLE
		s.mux.Unlock()
	}()
	log.Info("Restoring")
	err := s.init()
	if err != nil {
		return errors.Wrap(err, "Failed to init snapshot")
	}
	cmd := exec.Command(s.resticBin, "restore", "latest", "--verbose", "--path", s.infoHash, "--target", ".")
	cmd.Dir = s.dataDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	// out, err := cmd.CombinedOutput()
	if err != nil {
		log.WithError(err).Error("Failed to restore snapshot")
		// if !strings.Contains(err.Error(), "not found") {
		// 	return errors.Wrap(err, "Failed to restore snapshot")
		// }
	}
	log.Info("Restoring finished")
	// log.Infof("Restoring finished: %s", string(out))
	return nil
}
func (s *Snapshot) Backup() error {
	s.mux.Lock()
	s.state = SNAPSHOT_BACKUP
	defer func() {
		s.state = SNAPSHOT_IDLE
		s.mux.Unlock()
	}()
	err := s.init()
	if err != nil {
		return errors.Wrap(err, "Failed to init snapshot")
	}
	log.Info("Backing up")
	cmd := exec.Command(s.resticBin, "backup", "--tag", s.infoHash, s.infoHash)
	cmd.Dir = s.dataDir
	// out, err := cmd.CombinedOutput()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return errors.Wrap(err, "Failed to backup snapshot")
	}
	log.Info("Backing up finished")
	// log.Infof("Backing up finished: %s", string(out))
	return nil
}
