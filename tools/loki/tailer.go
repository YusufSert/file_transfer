package loki

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"
)

type Tail struct {
	l *slog.Logger
	r io.ReadCloser
	c *Client

	position uint64
	err      error
}

func NewTail(filePath string, l *slog.Logger) (*Tail, error) {
	r, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}

	// NewClient initialize goroutine waiting for new logs to be batched and send to loki
	c, err := NewClient(Config{
		URL:          "http://localhost:3100/loki/api/v1/push",
		BatchMaxSize: 1000,
		BatchMaxWait: 3 * time.Second,
		Labels:       map[string]string{"service_name": "pgm"},
	})
	if err != nil {
		return nil, err
	}

	a := &Tail{
		l: l,
		r: r,
		c: c,
	}

	return a, nil
}

func (a *Tail) Run(ctx context.Context) {
	a.l.Info("tail: starting to tail")
	a.tail(ctx)
}

func (a *Tail) tail(ctx context.Context) {
	backoff, backOffMax := time.Second*5, time.Second*60
	d := backoff

	r := bufio.NewReader(a.r)

	go func() {
		t := time.NewTimer(d)

		for {
			select {
			case <-t.C:

			case <-ctx.Done():
				a.err = ctx.Err()
				return
			}

			line, err := r.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				a.l.Error("log-tail: couldn't read new log file", "error", err)
				a.err = err
				break
			}
			if errors.Is(err, io.EOF) {
				d = d << 1 //backoff, backoff_factor, max_backoff
				if d > backOffMax {
					d = backOffMax
				}
			}

			a.position += uint64(len(line))

			a.c.Send(line)
			if err = a.c.Err(); err != nil {
				a.err = err
				a.l.Error("log-tail: error writing record to loki", "error", err)
			} else {
				d = backoff
			}

			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(d)
		}
	}()
}
func (a *Tail) Close() {
	a.c.Stop()
	a.r.Close()
}
func (a *Tail) Err() error {
	return a.err
}
