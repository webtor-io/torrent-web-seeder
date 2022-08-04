package services

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"

	cs "github.com/webtor-io/common-services"
)

type Snapshot struct {
	awsBucket                  string
	awsBucketSpread            bool
	awsConcurrency             int
	awsStatWriteDelay          time.Duration
	started                    bool
	t                          *torrent.Torrent
	mux                        sync.Mutex
	cMux                       sync.Mutex
	startThreshold             float64
	startFullDownloadThreshold float64
	torrentSizeLimit           int64
	writeTimeout               time.Duration
	downloadRatio              float64
	counter                    int64
	s3                         *cs.S3Client
	l                          *log.Entry
}

type CompletedPieces map[[20]byte]bool

func (cp CompletedPieces) Has(p *torrent.Piece) bool {
	h := p.Info().Hash()
	_, ok := map[[20]byte]bool(cp)[h]
	return ok
}

func (cp CompletedPieces) Add(p *torrent.Piece) {
	map[[20]byte]bool(cp)[p.Info().Hash()] = true
}

func (cp CompletedPieces) Len() int {
	return len(map[[20]byte]bool(cp))
}

func (cp CompletedPieces) FromBytes(data []byte) {
	for _, p := range split(data, 20) {
		var k [20]byte
		copy(k[:], p)
		cp[k] = true
	}
}

func (cp CompletedPieces) ToBytes() []byte {
	res := []byte{}
	for k := range cp {
		res = append(res, k[:]...)
	}
	return res
}

const (
	AWS_BUCKET                             = "aws-bucket"
	AWS_BUCKET_SPREAD                      = "aws-bucket-spread"
	AWS_CONCURRENCY                        = "aws-concurrency"
	AWS_STAT_WRITE_DELAY                   = "aws-stat-write-delay"
	USE_SNAPSHOT                           = "use-snapshot"
	SNAPSHOT_START_THRESHOLD               = "snapshot-start-threshold"
	SNAPSHOT_DOWNLOAD_RATIO                = "snapshot-download-ratio"
	SNAPSHOT_WRITE_TIMEOUT                 = "snapshot-write-timeout"
	SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD = "snapshot-start-full-download-threshold"
	SNAPSHOT_TORRENT_SIZE_LIMIT            = "snapshot-torrent-size-limit"
	DOWNLOADED_SIZE                        = "downloaded_size"
	TOUCH                                  = "touch"
	COMPLETED_PIECES                       = "completed_pieces"
)

func RegisterSnapshotFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.BoolFlag{
			Name:   USE_SNAPSHOT,
			EnvVar: "USE_SNAPSHOT",
		},
		cli.Float64Flag{
			Name:   SNAPSHOT_START_THRESHOLD,
			Value:  0.5,
			EnvVar: "SNAPSHOT_START_THRESHOLD",
		},
		cli.Float64Flag{
			Name:   SNAPSHOT_DOWNLOAD_RATIO,
			Value:  2.0,
			EnvVar: "SNAPSHOT_DOWNLOAD_RATIO",
		},
		cli.Float64Flag{
			Name:   SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD,
			Value:  0.75,
			EnvVar: "SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD",
		},
		cli.Int64Flag{
			Name:   SNAPSHOT_TORRENT_SIZE_LIMIT,
			Value:  10,
			EnvVar: "SNAPSHOT_TORRENT_SIZE_LIMIT",
		},
		cli.BoolFlag{
			Name:   AWS_BUCKET_SPREAD,
			EnvVar: "AWS_BUCKET_SPREAD",
		},
		cli.IntFlag{
			Name:   AWS_CONCURRENCY,
			Usage:  "AWS Concurrency",
			Value:  5,
			EnvVar: "AWS_CONCURRENCY",
		},
		cli.IntFlag{
			Name:   AWS_STAT_WRITE_DELAY,
			Usage:  "AWS Stat Write Delay (Sec)",
			Value:  60,
			EnvVar: "AWS_STAT_WRITE_DELAY",
		},
		cli.IntFlag{
			Name:   SNAPSHOT_WRITE_TIMEOUT,
			Value:  600,
			EnvVar: "SNAPSHOT_WRITE_TIMEOUT",
		},
		cli.StringFlag{
			Name:   AWS_BUCKET,
			Usage:  "AWS Bucket",
			Value:  "",
			EnvVar: "AWS_BUCKET",
		},
	)
}

func split(buf []byte, lim int) [][]byte {
	var chunk []byte
	chunks := make([][]byte, 0, len(buf)/lim+1)
	for len(buf) >= lim {
		chunk, buf = buf[:lim], buf[lim:]
		chunks = append(chunks, chunk)
	}
	if len(buf) > 0 {
		chunks = append(chunks, buf[:len(buf)])
	}
	return chunks
}

func NewSnapshot(c *cli.Context, t *torrent.Torrent, s3 *cs.S3Client, l *log.Entry) (*Snapshot, error) {
	if !c.Bool(USE_SNAPSHOT) {
		return nil, nil
	}
	if c.String(AWS_BUCKET) == "" {
		return nil, errors.Errorf("AWS Bucket can't be empty")
	}
	return &Snapshot{
		awsBucket:                  c.String(AWS_BUCKET),
		awsBucketSpread:            c.Bool(AWS_BUCKET_SPREAD),
		awsConcurrency:             c.Int(AWS_CONCURRENCY),
		awsStatWriteDelay:          time.Duration(c.Int(AWS_STAT_WRITE_DELAY)) * time.Second,
		writeTimeout:               time.Duration(c.Int(SNAPSHOT_WRITE_TIMEOUT)) * time.Second,
		startThreshold:             c.Float64(SNAPSHOT_START_THRESHOLD),
		startFullDownloadThreshold: c.Float64(SNAPSHOT_START_FULL_DOWNLOAD_THRESHOLD),
		torrentSizeLimit:           c.Int64(SNAPSHOT_TORRENT_SIZE_LIMIT),
		downloadRatio:              c.Float64(SNAPSHOT_DOWNLOAD_RATIO),
		t:                          t,
		s3:                         s3,
		l:                          l.WithField("bucket", c.String(AWS_BUCKET)),
	}, nil
}

func (s *Snapshot) fetchCompletedPieces(cl *s3.S3, t *torrent.Torrent) (*CompletedPieces, error) {
	key := COMPLETED_PIECES + "/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	st := CompletedPieces{}
	r, err := cl.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return &st, nil
		}
		return nil, errors.Wrapf(err, "failed to fetch completed pieces bucket=%v key=%v", s.awsBucket, key)
	}
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	st.FromBytes(data)
	l.Infof("fetch completed len=%v", st.Len())
	return &st, nil
}

func (s *Snapshot) fetchDownloadedSize(cl *s3.S3, t *torrent.Torrent) (int64, error) {
	key := DOWNLOADED_SIZE + "/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	r, err := cl.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return 0, nil
		}
		return 0, errors.Wrapf(err, "failed to fetch downloaded size bucket=%v key=%v", s.awsBucket, key)
	}
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return 0, err
	}
	i, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, err
	}
	l.Infof("size fetch completed size=%v", i)
	return int64(i), nil
}

func (s *Snapshot) storeDownloadedSize(cl *s3.S3, t *torrent.Torrent, i int64) error {
	key := DOWNLOADED_SIZE + "/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	l.Infof("store downloaded size size=%v", i)
	b := []byte(strconv.Itoa(int(i)))
	_, err := cl.PutObject(&s3.PutObjectInput{
		Bucket:     aws.String(s.awsBucket),
		Key:        aws.String(key),
		Body:       bytes.NewReader(b),
		ContentMD5: s.makeAWSMD5(b),
	})
	if err != nil {
		return errors.Wrapf(err, "failed to store downloaded size bucket=%v key=%v", s.awsBucket, key)
	}
	return nil
}

func (s *Snapshot) touch(cl *s3.S3, t *torrent.Torrent) error {
	timestamp := fmt.Sprintf("%v", time.Now().Unix())
	key := TOUCH + "/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	l.Infof("touch torrent timestamp=%v", timestamp)
	b := []byte(timestamp)
	_, err := cl.PutObject(&s3.PutObjectInput{
		Bucket:     aws.String(s.awsBucket),
		Key:        aws.String(key),
		Body:       bytes.NewReader(b),
		ContentMD5: s.makeAWSMD5(b),
	})
	if err != nil {
		return errors.Wrapf(err, "failed to touch torrent bucket=%v key=%v", s.awsBucket, key)
	}
	return nil
}

func (s *Snapshot) checkDone(cl *s3.S3, t *torrent.Torrent) (bool, error) {
	key := "done/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	l.Infof("check done marker")
	_, err := cl.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to check done marker bucket=%v key=%v", s.awsBucket, key)
	}
	return true, nil
}

func (s *Snapshot) storeCompletedPieces(cl *s3.S3, t *torrent.Torrent, st *CompletedPieces) error {
	if st.Len() == 0 {
		return nil
	}
	key := COMPLETED_PIECES + "/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	l.Infof("store completed pieces len=%v", st.Len())
	b := st.ToBytes()
	_, err := cl.PutObject(&s3.PutObjectInput{
		Bucket:     aws.String(s.awsBucket),
		Key:        aws.String(key),
		Body:       bytes.NewReader(b),
		ContentMD5: s.makeAWSMD5(b),
	})
	if err != nil {
		return errors.Wrap(err, "failed to store completed pieces")
	}
	if st.Len() == t.NumPieces() {
		key := "done/" + t.InfoHash().HexString()
		l := s.l.WithField("key", key)
		l.Infof("store done marker")
		b := []byte("")
		_, err := cl.PutObject(&s3.PutObjectInput{
			Bucket:     aws.String(s.awsBucket),
			Key:        aws.String(key),
			Body:       bytes.NewReader(b),
			ContentMD5: s.makeAWSMD5(b),
		})
		if err != nil {
			return errors.Wrapf(err, "failed to store done marker bucket=%v key=%v", s.awsBucket, key)
		}
	}
	return nil
}

func (s *Snapshot) storeTorrent(cl *s3.S3, t *torrent.Torrent) error {
	key := "torrents/" + t.InfoHash().HexString()
	l := s.l.WithField("key", key)
	l.Infof("store torrent")
	data, err := bencode.Marshal(t.Metainfo())
	if err != nil {
		return errors.Wrap(err, "failed to bencode torrent")
	}
	_, err = cl.PutObject(&s3.PutObjectInput{
		Bucket:     aws.String(s.awsBucket),
		Key:        aws.String(key),
		Body:       bytes.NewReader(data),
		ContentMD5: s.makeAWSMD5(data),
	})
	if err != nil {
		return errors.Wrapf(err, "failed to store torrent bucket=%v key=%v", s.awsBucket, key)
	}
	return nil
}

func (s *Snapshot) Add(i int64) {
	s.cMux.Lock()
	defer s.cMux.Unlock()
	s.counter += i
	if !s.started {
		s.started = true
		go s.start()
	}
}

func (s *Snapshot) start() error {
	s.l.Info("snapshot inited")
	cl := s.s3.Get()
	t := s.t
	if s.awsBucketSpread {
		_, err := cl.CreateBucket(&s3.CreateBucketInput{
			Bucket: aws.String(s.awsBucket),
		})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case s3.ErrCodeBucketAlreadyExists:
					s.l.WithError(err).Warn("master bucket already exists")
				case s3.ErrCodeBucketAlreadyOwnedByYou:
					s.l.WithError(err).Warn("master bucket already owned")
				default:
					return errors.Wrapf(err, "failed to create master bucket")
				}
			} else {
				return errors.Wrapf(err, "failed to create master bucket")
			}
		}
	}
	pieceBucket := s.awsBucket
	if s.awsBucketSpread {
		pieceBucket += "-" + t.InfoHash().HexString()[0:2]
	}

	if s.awsBucketSpread {
		_, err := cl.CreateBucket(&s3.CreateBucketInput{
			Bucket: aws.String(pieceBucket),
		})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case s3.ErrCodeBucketAlreadyExists:
					s.l.WithError(err).Warn("piece bucket already exists")
				case s3.ErrCodeBucketAlreadyOwnedByYou:
					s.l.WithError(err).Warn("piece bucket already owned")
				default:
					return errors.Wrapf(err, "failed to create piece bucket")
				}
			} else {
				return errors.Wrapf(err, "failed to create piece bucket")
			}
		}
	}
	if t.Length()/1024/1024/1024 > s.torrentSizeLimit {
		s.l.Infof("do not cache large torrent")
		return nil
	}
	err := s.touch(cl, t)
	if err != nil {
		return errors.Wrap(err, "failed to touch torrent")
	}
	done, err := s.checkDone(cl, t)
	if err != nil {
		return errors.Wrap(err, "failed to check done marker")
	}
	if done {
		s.l.Info("already fully snapshotted")
		return nil
	}
	prevDownloadedSize, err := s.fetchDownloadedSize(cl, t)
	if err != nil {
		return errors.Wrap(err, "failed to fetch download size")
	}
	cp, err := s.fetchCompletedPieces(cl, t)
	if err != nil {
		return errors.Wrap(err, "failed to fetch completed pieces")
	}
	err = s.storeTorrent(cl, t)
	if err != nil {
		return errors.Wrap(err, "failed to store torrent")
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	ch := make(chan *torrent.Piece)
	// errCh := make(chan error)
	var completedRatio float64
	var totalDownloadRatio float64
	var currDownloadedSize int64
	var currCompletedNum int
	fullDownloadStarted := false
	fullSnapshotStarted := false
	started := false
	lastDownloadSizeWrittenAt := time.Now()
	lastCompletedPiecesWrittenAt := time.Now()
	writtenAt := time.Now()
	downloadedSize := s.counter
	go func() {
		for range ticker.C {
			if !fullSnapshotStarted && time.Now().After(writtenAt.Add(s.writeTimeout)) && totalDownloadRatio >= s.downloadRatio {
				fullSnapshotStarted = true
				s.l.Info("starting full snapshot")
				t.DownloadAll()
			}
			completedNum := 0
			if s.counter > downloadedSize {
				downloadedSize = s.counter
				writtenAt = time.Now()
			}
			s.mux.Lock()
			for i := 0; i < t.NumPieces(); i++ {
				ps := t.PieceState(i)
				p := t.Piece(i)
				if cp.Has(p) || (!cp.Has(p) && ps.Complete) {
					completedNum++
				}
			}
			s.mux.Unlock()
			if downloadedSize > currDownloadedSize+1024*1024*10 && !time.Now().Before(lastDownloadSizeWrittenAt.Add(s.awsStatWriteDelay)) {
				err := s.storeDownloadedSize(cl, t, prevDownloadedSize+currDownloadedSize)
				if err != nil {
					s.l.WithError(err).Warn("failed to store downloaded size")
				} else {
					currDownloadedSize = downloadedSize
					lastDownloadSizeWrittenAt = time.Now()
				}
			}
			if completedNum > currCompletedNum+10 && !time.Now().Before(lastCompletedPiecesWrittenAt.Add(s.awsStatWriteDelay)) {
				err := s.storeCompletedPieces(cl, t, cp)
				if err != nil {
					s.l.WithError(err).Warn("failed to store completed pieces")
				} else {
					currCompletedNum = completedNum
					lastCompletedPiecesWrittenAt = time.Now()
				}
			}
			completedRatio = float64(completedNum) / float64(t.NumPieces())
			totalDownloadRatio = float64(prevDownloadedSize+downloadedSize) / float64(t.Length())
			if totalDownloadRatio >= s.downloadRatio && completedRatio >= s.startThreshold {
				if !started {
					started = true
					s.l.Infof("starting making snapshot at %v%%", s.startThreshold*100)
				}
				np := []*torrent.Piece{}
				s.mux.Lock()
				for i := 0; i < t.NumPieces(); i++ {
					ps := t.PieceState(i)
					p := t.Piece(i)
					if p != nil && !cp.Has(p) && ps.Complete {
						np = append(np, p)
					}
				}
				s.mux.Unlock()
				for _, p := range np {
					ch <- p
				}
			}
			if !fullSnapshotStarted && !fullDownloadStarted && completedRatio >= s.startFullDownloadThreshold {
				fullDownloadStarted = true
				s.l.Infof("starting full download at %v%%", s.startFullDownloadThreshold*100)
				t.DownloadAll()
			}
			if cp.Len() == t.NumPieces() {
				close(ch)
				break
			}
		}
	}()
	s.storePieces(cl, t, cp, ch, pieceBucket)
	err = s.storeCompletedPieces(cl, t, cp)
	if err != nil {
		s.l.WithError(err).Warn("failed to store completed pieces")
	}
	return nil
}

func (s *Snapshot) makeAWSMD5(b []byte) *string {
	h := md5.Sum(b)
	m := base64.StdEncoding.EncodeToString(h[:])
	return aws.String(m)
}

func (s *Snapshot) storePieces(cl *s3.S3, t *torrent.Torrent, cp *CompletedPieces, ch chan *torrent.Piece, pb string) {
	var wg sync.WaitGroup
	for i := 0; i < s.awsConcurrency; i++ {
		wg.Add(1)
		l := s.l.WithField("thread", i)
		go func() {
			for p := range ch {
				b := make([]byte, p.Info().Length())
				size, err := p.Storage().ReadAt(b, 0)

				if size != int(p.Info().Length()) && err != nil && err != io.EOF {
					l.WithError(err).Errorf("failed to read piece index=%v", p.Info().Index())
					continue
				}
				key := t.InfoHash().HexString() + "/" + p.Info().Hash().HexString()

				_, err = cl.PutObject(&s3.PutObjectInput{
					Bucket:     aws.String(pb),
					Key:        aws.String(key),
					Body:       bytes.NewReader(b),
					ContentMD5: s.makeAWSMD5(b),
				})

				if err != nil {
					l.WithError(err).Errorf("failed to write piece index=%v", p.Info().Index())
					continue
				}

				s.mux.Lock()
				cp.Add(p)
				s.mux.Unlock()

				l := l.WithField("key", key)
				l.Infof("stored piece to S3 index=%v", p.Info().Index())
			}
			wg.Done()
		}()
	}
	wg.Wait()
}
