package services

import (
	"github.com/go-pg/migrations/v8"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type PGMigration struct {
	db *PG
}

func NewPGMigration(db *PG) *PGMigration {
	return &PGMigration{db: db}
}

func (s *PGMigration) Run(a ...string) error {
	db := s.db.Get()
	col := migrations.NewCollection()
	col.DiscoverSQLMigrations("migrations")
	_, _, err := col.Run(db, "init")
	if err != nil {
		return errors.Wrap(err, "Failed to init DB PGMigrations")
	}
	oldVersion, newVersion, err := col.Run(db, a...)
	if err != nil {
		return errors.Wrap(err, "Failed to perform PGMigration")
	}
	if newVersion != oldVersion {
		log.Infof("DB migrated from version %d to %d", oldVersion, newVersion)
	} else {
		log.Infof("DB PGMigration version is %d", oldVersion)
	}
	return nil
}

func MakePGMigrationCMD() cli.Command {
	migrateCmd := cli.Command{
		Name:    "migrate",
		Aliases: []string{"m"},
		Usage:   "Migrates database",
	}
	configurePGMigration(&migrateCmd)
	return migrateCmd
}

func configurePGMigration(c *cli.Command) {
	upCmd := cli.Command{
		Name:    "up",
		Usage:   "Runs all available migrations",
		Aliases: []string{"u"},
		Action: func(c *cli.Context) error {
			return pgMigrate(c, "up")
		},
	}
	downCmd := cli.Command{
		Name:    "down",
		Usage:   "Reverts last migration",
		Aliases: []string{"d"},
		Action: func(c *cli.Context) error {
			return pgMigrate(c, "down")
		},
	}
	resetCmd := cli.Command{
		Name:    "reset",
		Usage:   "Reverts all migrations",
		Aliases: []string{"r"},
		Action: func(c *cli.Context) error {
			return pgMigrate(c, "reset")
		},
	}
	versionCmd := cli.Command{
		Name:    "version",
		Usage:   "Prints current db version",
		Aliases: []string{"v"},
		Action: func(c *cli.Context) error {
			return pgMigrate(c, "version")
		},
	}
	c.Subcommands = []cli.Command{upCmd, downCmd, resetCmd, versionCmd}
	for k, _ := range c.Subcommands {
		configureSubPGMigration(&c.Subcommands[k])
	}
}
func configureSubPGMigration(c *cli.Command) {
	c.Flags = RegisterPGFlags(c.Flags)
}

func pgMigrate(c *cli.Context, a ...string) error {
	// Setting DB
	db := NewPG(c)
	defer db.Close()

	// Setting PGMigrations
	m := NewPGMigration(db)
	return m.Run(a...)
}
