package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/gosuri/uiprogress/util/strutil"

	humanize "github.com/dustin/go-humanize"
	"github.com/gosuri/uiprogress"

	"github.com/urfave/cli"
	"google.golang.org/grpc"

	pb "github.com/webtor-io/torrent-web-seeder/torrent-web-seeder"
)

func render(cl pb.TorrentWebSeederClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := cl.Files(ctx, &pb.FilesRequest{})
	logrus.Infof("%v", r)
	if err != nil {
		return err
	}
	uiprogress.Start()
	var maxLen int
	for _, f := range r.Files {
		if maxLen < len(f.Path) {
			maxLen = len(f.Path)
		}
	}
	for _, f := range r.Files {
		err = renderFile(cl, f.GetPath(), maxLen)
		if err != nil {
			return err
		}
	}
	return nil
}

func renderFile(cl pb.TorrentWebSeederClient, path string, maxLen int) error {
	stat, err := getStat(cl, path)
	if err != nil {
		return err
	}
	bar := uiprogress.AddBar(int(stat.GetTotal()))
	bar.PrependFunc(func(*uiprogress.Bar) string {
		return strutil.Resize(path, uint(maxLen))
	})
	bar.AppendCompleted()
	bar.AppendFunc(func(*uiprogress.Bar) (ret string) {
		switch stat.GetStatus() {
		case pb.StatReply_INITIALIZATION:
			return "init"
		case pb.StatReply_RESTORING:
			return "restoring"
		case pb.StatReply_SEEDING:
			return fmt.Sprintf("seeding (%s/%s)", humanize.Bytes(uint64(stat.GetCompleted())), humanize.Bytes(uint64(stat.GetTotal())))
		case pb.StatReply_IDLE:
			return fmt.Sprintf("seeding (%s/%s)", humanize.Bytes(uint64(stat.GetCompleted())), humanize.Bytes(uint64(stat.GetTotal())))
		case pb.StatReply_TERMINATED:
			return "terminated"
		}
		return ""
	})
	go func() {
		ctx := context.Background()
		c, err := cl.StatStream(ctx, &pb.StatRequest{Path: path})
		if err != nil {
			return
		}
		for {
			stat, err := c.Recv()
			if err != nil {
				break
			}
			// logrus.Infof("%v", stat)
			bar.Set(int(stat.GetCompleted()))
		}
	}()
	return nil
}

func getStat(cl pb.TorrentWebSeederClient, path string) (*pb.StatReply, error) {
	ctx := context.Background()
	return cl.Stat(ctx, &pb.StatRequest{Path: path})
}

func main() {
	app := cli.NewApp()
	app.Name = "torrent-web-seeder-cli"
	app.Usage = "interacts with torrent web seeder"
	app.Version = "0.0.1"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "host, H",
			Usage:  "hostname of the torrent web seeder",
			Value:  "localhost",
			EnvVar: "TORRENT_WEB_SEEDER_HOST",
		},
		cli.IntFlag{
			Name:   "port, P",
			Usage:  "port of the torrent web seeder",
			Value:  50051,
			EnvVar: "TORRENT_WEB_SEEDER_PORT",
		},
	}
	app.Action = func(c *cli.Context) error {
		addr := fmt.Sprintf("%s:%d", c.String("host"), c.Int("port"))
		conn, err := grpc.Dial(addr, grpc.WithInsecure())
		if err != nil {
			return err
		}
		defer conn.Close()
		cl := pb.NewTorrentWebSeederClient(conn)

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		err = render(cl)
		if err != nil {
			return err
		}
		<-sigs
		return nil
	}
	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
