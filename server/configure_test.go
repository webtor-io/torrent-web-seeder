package main

import (
	"flag"
	"testing"

	"github.com/urfave/cli"
	s "github.com/webtor-io/torrent-web-seeder/server/services"
)

// The seeder registers ACCEPT_USER_UPLOADS: c.Bool of an unregistered flag
// reads false, so without it uploads would stay off whatever the env says.
func TestConfigureRegistersUploadFlag(t *testing.T) {
	app := cli.NewApp()
	configure(app)
	for _, f := range app.Flags {
		if f.GetName() == s.AcceptUserUploadsFlag {
			return
		}
	}
	t.Fatalf("no --%s flag", s.AcceptUserUploadsFlag)
}

// Without a torrent-store host there are no private records (nil, disabled),
// rather than a store that fails every lookup; with one, they come from it.
func TestPrivateRecordsFollowStoreHost(t *testing.T) {
	for host, want := range map[string]bool{"": false, "torrent-store": true} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String(s.TorrentStoreHostFlag, host, "")
		c := cli.NewContext(nil, fs, nil)
		if got := privateRecords(c, s.NewTorrentStore(c)) != nil; got != want {
			t.Errorf("host %q: records %v, want %v", host, got, want)
		}
	}
}
