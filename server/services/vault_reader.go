package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

type httpReadSeekCloser struct {
	url  string
	cl   *http.Client
	ctx  context.Context
	pos  int64
	size int64
	body io.ReadCloser
}

func newHTTPReadSeekCloser(ctx context.Context, cl *http.Client, url string) (*httpReadSeekCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from vault HEAD %s", resp.StatusCode, url)
	}
	return &httpReadSeekCloser{
		url:  url,
		cl:   cl,
		ctx:  ctx,
		size: resp.ContentLength,
	}, nil
}

func (r *httpReadSeekCloser) Read(p []byte) (int, error) {
	if r.body == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *httpReadSeekCloser) open() error {
	req, err := http.NewRequestWithContext(r.ctx, "GET", r.url, nil)
	if err != nil {
		return err
	}
	if r.pos > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
	}
	resp, err := r.cl.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return fmt.Errorf("unexpected status %d from vault GET %s", resp.StatusCode, r.url)
	}
	r.body = resp.Body
	return nil
}

func (r *httpReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("negative seek position: %d", newPos)
	}
	if newPos != r.pos {
		r.closeBody()
		r.pos = newPos
	}
	return r.pos, nil
}

func (r *httpReadSeekCloser) closeBody() {
	if r.body != nil {
		r.body.Close()
		r.body = nil
	}
}

func (r *httpReadSeekCloser) Close() error {
	r.closeBody()
	return nil
}
