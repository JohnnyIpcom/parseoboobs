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

		if err := p.downloadPreviews(ctx, progress, boobs); err != nil {
			rerr = err
		}
	}

	pool.JobQueue <- func() {
		defer pool.JobDone()

		if err := p.downloadHiRes(ctx, progress, boobs); err != nil {
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

func (p *Parser) downloadFile(ctx context.Context, bar *mpb.Bar, filepath string, url string) error {
	defer bar.Increment()

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

func (p *Parser) downloadPreviews(ctx context.Context, progress *mpb.Progress, boobs []Boob) error {
	os.Mkdir("previews", os.ModePerm)

	pool := grpool.NewPool(10, 10)
	defer pool.Release()

	pool.WaitCount(len(boobs))

	name := "\x1b[32mpreviews:\x1b[0m"
	bar := progress.AddBar(int64(len(boobs)), mpb.BarStyle("[=>-|"),
		mpb.PrependDecorators(
			decor.Name(name, decor.WC{W: 10, C: decor.DidentRight}),
			decor.CountersNoUnit("%5d / %5d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(decor.WC{W: 5}),
		),
	)

	for i := 0; i < len(boobs); i++ {
		b := boobs[i]

		pool.JobQueue <- func() {
			defer pool.JobDone()

			select {
			case <-ctx.Done():
				bar.Increment()
				return
			default:
			}

			urlPreview, _ := p.mediaURL.Parse(b.Preview)
			err := p.downloadFile(
				ctx,
				bar,
				fmt.Sprintf("previews/%05d%s", b.ID, filepath.Ext(b.Preview)),
				urlPreview.String(),
			)

			if err != nil {
				return
			}
		}
	}

	pool.WaitAll()
	return nil
}

func (p *Parser) downloadHiRes(ctx context.Context, progress *mpb.Progress, boobs []Boob) error {
	os.Mkdir("hires", os.ModePerm)

	pool := grpool.NewPool(10, 10)
	defer pool.Release()

	pool.WaitCount(len(boobs))

	name := "\x1b[32mhires:\x1b[0m"
	bar := progress.AddBar(int64(len(boobs)), mpb.BarStyle("[=>-|"),
		mpb.PrependDecorators(
			decor.Name(name, decor.WC{W: 10, C: decor.DidentRight}),
			decor.CountersNoUnit("%5d / %5d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(decor.WC{W: 5}),
		),
	)

	for i := 0; i < len(boobs); i++ {
		b := boobs[i]

		pool.JobQueue <- func() {
			defer pool.JobDone()

			select {
			case <-ctx.Done():
				bar.Increment()
				return
			default:
			}

			urlHiRes, _ := p.mediaURL.Parse(fmt.Sprintf("%s/%05d%s", p.tag, b.ID, filepath.Ext(b.Preview)))
			err := p.downloadFile(
				ctx,
				bar,
				fmt.Sprintf("hiRes/%05d%s", b.ID, filepath.Ext(b.Preview)),
				urlHiRes.String(),
			)

			if err != nil {
				return
			}
		}
	}

	pool.WaitAll()
	return nil
}
