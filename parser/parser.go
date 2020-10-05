package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/ivpusic/grpool"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
)

const (
	defaultURL      = "http://api.oboobs.ru/boobs/"
	defaultMediaURL = "http://media.oboobs.ru/"
	defaultTag      = "boobs"
)

// Boob ...
type Boob struct {
	Model   string `json:"model,omitempty"`
	Preview string `json:"preview"`
	ID      int    `json:"id"`
	Rank    int    `json:"rank"`
	Author  string `json:"author,omitempty"`
}

// Count ...
type Count struct {
	Count int `json:"count"`
}

// Parser ...
type Parser struct {
	client   *http.Client
	url      *url.URL
	mediaURL *url.URL
	tag      string
}

func newDefaultParser(client *http.Client) *Parser {
	if client == nil {
		client = http.DefaultClient
	}

	baseURL, _ := url.Parse(defaultURL)
	baseMediaURL, _ := url.Parse(defaultMediaURL)
	return &Parser{
		client:   client,
		url:      baseURL,
		mediaURL: baseMediaURL,
		tag:      defaultTag,
	}
}

// SetURL ...
func SetURL(baseURL string) Option {
	return func(parser *Parser) error {
		u, err := url.Parse(baseURL)
		if err != nil {
			return err
		}

		parser.url = u
		return nil
	}
}

// SetMediaURL ...
func SetMediaURL(baseMediaURL string) Option {
	return func(parser *Parser) error {
		u, err := url.Parse(baseMediaURL)
		if err != nil {
			return err
		}

		parser.mediaURL = u
		return nil
	}
}

// SetTag ...
func SetTag(tag string) Option {
	return func(parser *Parser) error {
		parser.tag = tag
		return nil
	}
}

// OptionOn ...
func OptionOn(option Option, condition func() bool) Option {
	if condition() {
		return option
	}

	return nil
}

// Option ...
type Option func(*Parser) error

// NewParser ...
func NewParser(httpClient *http.Client, options ...Option) (*Parser, error) {
	parser := newDefaultParser(httpClient)
	for _, option := range options {
		if option != nil {
			err := option(parser)
			if err != nil {
				return nil, fmt.Errorf("Can't apply option, err = %w", err)
			}
		}
	}

	return parser, nil
}

// Close ...
func (p *Parser) Close() {
}

// Download ...
func (p *Parser) Download(ctx context.Context, boobs []Boob) error {
	pool := grpool.NewPool(10, 10)
	defer pool.Release()

	pool.WaitCount(2)

	progress := mpb.New(
		mpb.WithWidth(60),
		mpb.WithRefreshRate(180*time.Millisecond),
	)

	var rerr error
	pool.JobQueue <- func() {
		defer pool.JobDone()

		urls := make([]string, len(boobs))
		for i, b := range boobs {
			urls[i] = b.Preview
		}

		if err := p.downloadURLs(ctx, progress, "previews", urls); err != nil {
			rerr = err
		}
	}

	pool.JobQueue <- func() {
		defer pool.JobDone()

		urls := make([]string, len(boobs))
		for i, b := range boobs {
			urls[i] = fmt.Sprintf("%s/%05d%s", p.tag, b.ID, filepath.Ext(b.Preview))
		}

		if err := p.downloadURLs(ctx, progress, "hires", urls); err != nil {
			rerr = err
		}
	}

	pool.WaitAll()

	progress.Wait()

	return rerr
}

// WalkSite ...
func (p *Parser) WalkSite(ctx context.Context, maxCount ...int) ([]Boob, error) {
	progress := mpb.New(
		mpb.WithWidth(60),
		mpb.WithRefreshRate(180*time.Millisecond),
	)

	countURL, _ := p.url.Parse("count")
	reqc, err := http.NewRequest(http.MethodGet, countURL.String(), nil)
	if err != nil {
		return nil, err
	}

	reqc = reqc.WithContext(ctx)

	respc, err := p.client.Do(reqc)
	if err != nil {
		return nil, err
	}

	defer func() {
		rerr := respc.Body.Close()
		if err == nil {
			err = rerr
		}
	}()

	var counts []Count
	if err := json.NewDecoder(respc.Body).Decode(&counts); err != nil {
		return nil, err
	}

	count := counts[0].Count
	if len(maxCount) > 0 && maxCount[0] > 0 {
		count = maxCount[0]
	}

	name := "\x1b[32mparsing:\x1b[0m"
	bar := progress.AddBar(int64(count), mpb.BarStyle("[=>-|"),
		mpb.PrependDecorators(
			decor.Name(name, decor.WC{W: 10, C: decor.DidentRight}),
			decor.CountersNoUnit("%5d / %5d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(decor.WC{W: 4}),
		),
	)

	result := make([]Boob, 0)

	const maxinc int = 100
	for i := 0; i < count; i += maxinc {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var inc int
		if i+maxinc < count {
			inc = maxinc
		} else {
			inc = count - i
		}

		boobsURL, _ := p.url.Parse(fmt.Sprintf("%d/%d", i, inc))
		reqp, err := http.NewRequest(http.MethodGet, boobsURL.String(), nil)
		if err != nil {
			return nil, err
		}

		reqp = reqp.WithContext(ctx)

		resp, err := p.client.Do(reqp)
		if err != nil {
			return nil, err
		}

		defer func() {
			rerr := resp.Body.Close()
			if err == nil {
				err = rerr
			}
		}()

		var boobs []Boob
		if err := json.NewDecoder(resp.Body).Decode(&boobs); err != nil {
			return nil, err
		}

		result = append(result, boobs...)
		bar.IncrBy(inc)

		time.Sleep(200 * time.Millisecond)
	}

	progress.Wait()

	return result, nil
}

type readerFunc func(p []byte) (n int, err error)

func (rf readerFunc) Read(p []byte) (n int, err error) {
	return rf(p)
}

func (p *Parser) downloadURL(ctx context.Context, filepath string, url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	req = req.WithContext(ctx)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, readerFunc(func(b []byte) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return resp.Body.Read(b)
		}
	}))

	if err != nil {
		return err
	}

	return nil
}

func (p *Parser) downloadURLs(ctx context.Context, progress *mpb.Progress, dir string, urls []string) error {
	os.Mkdir(dir, os.ModePerm)

	pool := grpool.NewPool(10, 10)
	defer pool.Release()

	pool.WaitCount(len(urls))

	name := fmt.Sprintf("\x1b[32m%s:\x1b[0m", dir)
	bar := progress.AddBar(int64(len(urls)), mpb.BarStyle("[=>-|"),
		mpb.PrependDecorators(
			decor.Name(name, decor.WC{W: 10, C: decor.DidentRight}),
			decor.CountersNoUnit("%5d / %5d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(decor.WC{W: 5}),
		),
	)

	var merr *multierror.Error
	for _, url := range urls {
		pool.JobQueue <- func() {
			defer func() {
				bar.Increment()

				pool.JobDone()
			}()

			select {
			case <-ctx.Done():
				return
			default:
			}

			u, err := p.mediaURL.Parse(url)
			if err != nil {
				merr = multierror.Append(merr, err)
				return
			}

			err = p.downloadURL(ctx, fmt.Sprintf("%s/%s", dir, filepath.Base(url)), u.String())
			if err != nil {
				merr = multierror.Append(merr, err)
			}
		}
	}

	pool.WaitAll()
	return merr.ErrorOrNil()
}
