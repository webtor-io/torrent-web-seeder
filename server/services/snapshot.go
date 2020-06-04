package services

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
)

type Snapshot struct {
	awsAccessKeyID     string
	awsSecretAccessKey string
	awsBucket          string
	awsEndpoint        string
	awsRegion          string
	awsSession         *session.Session
	awsClient          *s3.S3
	awsConcurrency     int
	stop               bool
	start              bool
	stopCh             chan (bool)
	err                error
	t                  *Torrent
	mux                sync.Mutex
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
	AWS_ACCESS_KEY_ID     = "aws-access-key-id"
	AWS_SECRET_ACCESS_KEY = "aws-secret-access-key"
	AWS_BUCKET            = "aws-bucket"
	AWS_ENDPOINT          = "aws-endpoint"
	AWS_REGION            = "aws-region"
	AWS_CONCURRENCY       = "aws-concurrency"
	USE_SNAPSHOT          = "use-snapshot"
)

func RegisterSnapshotFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.BoolFlag{
		Name:   USE_SNAPSHOT,
		EnvVar: "USE_SNAPSHOT",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:   AWS_CONCURRENCY,
		Usage:  "AWS Concurrency",
		Value:  5,
		EnvVar: "AWS_CONCURRENCY",
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
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_BUCKET,
		Usage:  "AWS Bucket",
		Value:  "",
		EnvVar: "AWS_BUCKET",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_ENDPOINT,
		Usage:  "AWS Endpoint",
		Value:  "",
		EnvVar: "AWS_ENDPOINT",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   AWS_REGION,
		Usage:  "AWS Region",
		Value:  "",
		EnvVar: "AWS_REGION",
	})
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

func NewSnapshot(c *cli.Context, t *Torrent) (*Snapshot, error) {
	if c.Bool(USE_SNAPSHOT) == false {
		return nil, nil
	}
	if c.String(AWS_ACCESS_KEY_ID) == "" {
		return nil, errors.Errorf("AWS Access Key ID can't be empty")
	}
	if c.String(AWS_SECRET_ACCESS_KEY) == "" {
		return nil, errors.Errorf("AWS Secret Access Key can't be empty")
	}
	if c.String(AWS_BUCKET) == "" {
		return nil, errors.Errorf("AWS Bucket can't be empty")
	}
	if c.String(AWS_REGION) == "" {
		return nil, errors.Errorf("AWS Region can't be empty")
	}
	return &Snapshot{awsAccessKeyID: c.String(AWS_ACCESS_KEY_ID), awsSecretAccessKey: c.String(AWS_SECRET_ACCESS_KEY),
		awsBucket: c.String(AWS_BUCKET), awsConcurrency: c.Int(AWS_CONCURRENCY), stop: false, t: t,
		awsEndpoint: c.String(AWS_ENDPOINT), awsRegion: c.String(AWS_REGION), start: false}, nil
}

func (s *Snapshot) client() *s3.S3 {
	if s.awsClient != nil {
		return s.awsClient
	}
	s.awsClient = s3.New(s.session())
	return s.awsClient
}

func (s *Snapshot) session() *session.Session {
	if s.awsSession != nil {
		return s.awsSession
	}
	c := &aws.Config{
		Credentials: credentials.NewStaticCredentials(s.awsAccessKeyID, s.awsSecretAccessKey, ""),
		Endpoint:    aws.String(s.awsEndpoint),
		Region:      aws.String(s.awsRegion),
		// DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
	}
	s.awsSession = session.New(c)
	return s.awsSession
}

func (s *Snapshot) fetchCompletedPieces(cl *s3.S3, t *torrent.Torrent) (*CompletedPieces, error) {
	st := CompletedPieces{}
	r, err := cl.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(t.InfoHash().HexString() + "/completed_pieces"),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == s3.ErrCodeNoSuchKey {
			return &st, nil
		}
		return nil, errors.Wrap(err, "Failed to fetch completed pieces")
	}
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	st.FromBytes(data)
	return &st, nil
}

func (s *Snapshot) touch(cl *s3.S3, t *torrent.Torrent) error {
	key := "touch/" + t.InfoHash().HexString()
	_, err := cl.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(fmt.Sprintf("%v", time.Now().Unix()))),
	})
	if err != nil {
		return errors.Wrapf(err, "Failed to touch torrent key=%v", key)
	}
	return nil
}

func (s *Snapshot) storeCompletedPieces(cl *s3.S3, t *torrent.Torrent, st *CompletedPieces) error {
	_, err := cl.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String("completed_pieces/" + t.InfoHash().HexString()),
		Body:   bytes.NewReader(st.ToBytes()),
	})
	if err != nil {
		return errors.Wrap(err, "Failed to store completed pieces")
	}
	return nil
}

func (s *Snapshot) storeTorrent(cl *s3.S3, t *torrent.Torrent) error {
	path := "torrents/" + t.InfoHash().HexString()
	log.Infof("Store torrent path=%v", path)
	data, err := bencode.Marshal(t.Metainfo())
	if err != nil {
		return errors.Wrap(err, "Failed to becnode torrent")
	}
	_, err = cl.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.awsBucket),
		Key:    aws.String(path),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return errors.Wrap(err, "Failed to store torrent")
	}
	return nil
}

func (s *Snapshot) Start() error {
	log.Info("Starting snapshot")
	s.stopCh = make(chan (bool))
	cl := s.client()
	t, err := s.t.Get()
	if err != nil {
		return errors.Wrap(err, "Failed to fetch torrent")
	}
	_, err = cl.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(s.awsBucket),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyExists:
				log.WithError(err).Warn("Master bucket already exists")
			case s3.ErrCodeBucketAlreadyOwnedByYou:
				log.WithError(err).Warn("Master bucket already owned")
			default:
				return errors.Wrapf(err, "Failed to create master bucket")
			}
		} else {
			return errors.Wrapf(err, "Failed to create master bucket")
		}
	}
	pieceBucket := s.awsBucket + "-" + t.InfoHash().HexString()[0:2]

	_, err = cl.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(pieceBucket),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyExists:
				log.WithError(err).Warn("Piece bucket already exists")
			case s3.ErrCodeBucketAlreadyOwnedByYou:
				log.WithError(err).Warn("Piece bucket already owned")
			default:
				return errors.Wrapf(err, "Failed to create piece bucket")
			}
		} else {
			return errors.Wrapf(err, "Failed to create piece bucket")
		}
	}
	cp, err := s.fetchCompletedPieces(cl, t)
	if err != nil {
		return errors.Wrap(err, "Failed to fetch completed pieces")
	}
	err = s.storeTorrent(cl, t)
	if err != nil {
		return errors.Wrap(err, "Failed to store torrent")
	}
	err = s.touch(cl, t)
	if err != nil {
		return errors.Wrap(err, "Failed to touch torrent")
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	ch := make(chan *torrent.Piece)
	// errCh := make(chan error)
	go func() {
		for range ticker.C {
			if s.stop {
				close(ch)
				break
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
	}()
	ticker2 := time.NewTicker(30 * time.Second)
	defer ticker2.Stop()
	go func() {
		for range ticker2.C {
			if s.stop {
				break
			}
			err := s.storeCompletedPieces(cl, t, cp)
			if err != nil {
				log.WithError(err).Error("Failed to store completed pieces")
			}
		}
	}()
	s.start = true
	s.storePieces(cl, t, cp, ch, pieceBucket)
	err = s.storeCompletedPieces(cl, t, cp)
	if err != nil {
		log.WithError(err).Error("Failed to store completed pieces")
	}
	close(s.stopCh)
	return nil
}

func (s *Snapshot) storePieces(cl *s3.S3, t *torrent.Torrent, cp *CompletedPieces, ch chan *torrent.Piece, pb string) {
	var wg sync.WaitGroup
	for i := 0; i < s.awsConcurrency; i++ {
		wg.Add(1)
		logger := log.WithField("thread", i)
		go func() {
			for p := range ch {
				b := make([]byte, p.Info().Length())
				size, err := p.Storage().ReadAt(b, 0)

				if size != int(p.Info().Length()) && err != nil && err != io.EOF {
					logger.WithError(err).Errorf("Failed to read piece index=%v", p.Info().Index())
					continue
				}

				_, err = cl.PutObject(&s3.PutObjectInput{
					Bucket: aws.String(pb),
					Key:    aws.String(t.InfoHash().HexString() + "/" + p.Info().Hash().HexString()),
					Body:   bytes.NewReader(b),
				})

				if err != nil {
					logger.WithError(err).Errorf("Failed to write piece index=%v", p.Info().Index())
					continue
				}

				s.mux.Lock()
				cp.Add(p)
				s.mux.Unlock()

				logger.Infof("Stored piece to S3 index=%v", p.Info().Index())
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func (s *Snapshot) Close() {
	log.Info("Snapshot closing")
	if s.start {
		s.stop = true

		select {
		case <-s.stopCh:
		case <-time.After(1 * time.Minute):
		}

	}
	log.Info("Snapshot closed")
}
