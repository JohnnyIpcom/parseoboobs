package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"parseoboobs/parser"
)

func main() {
	var count int
	flag.IntVar(&count, "count", -1, "download last %count% boobs, -1 or nothing for all")

	var URL string
	flag.StringVar(&URL, "url", "", "api url")

	var mediaURL string
	flag.StringVar(&mediaURL, "mediaurl", "", "media url")

	var tag string
	flag.StringVar(&tag, "tag", "", "db tag")

	flag.Parse()
	flag.PrintDefaults()

	fmt.Println()

	p, err := parser.NewParser(
		nil,
		parser.OptionOn(parser.SetURL(URL), func() bool { return len(URL) > 0 }),
		parser.OptionOn(parser.SetMediaURL(mediaURL), func() bool { return len(mediaURL) > 0 }),
		parser.OptionOn(parser.SetTag(tag), func() bool { return len(tag) > 0 }),
	)

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		cancel()
	}()

	boobs, err := p.WalkSite(ctx, count)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	if err := p.Download(ctx, boobs); err != nil {
		fmt.Println(err.Error())
		return
	}
}
