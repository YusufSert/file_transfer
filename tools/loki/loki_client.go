package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type Client struct {
	in chan *value
	c  http.Client
	Config
	err  error
	stop func()
}

func NewClient(cfg Config) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		make(chan *value, 10000),
		http.Client{Timeout: 3 * time.Second},
		cfg,
		nil,
		cancel,
	}

	go client.run(ctx)

	return client, nil
}

func (c *Client) Send(log string) {
	c.in <- &value{
		Ts:   time.Now(),
		Line: log,
	}
}

func (c *Client) run(ctx context.Context) {
	batch := make([][]any, 0)

	d := c.BatchMaxWait
	t := time.NewTimer(d)

	for {
		select {
		case <-ctx.Done():
			return

		case v := <-c.in:
			batch = append(batch, v.List())
			if len(batch) <= c.BatchMaxSize {
				continue
			}
			c.send(batch)
			batch = make([][]any, 0)

		case <-t.C:
			if len(batch) > 0 {
				c.send(batch)
				batch = make([][]any, 0)
			}
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d)
	}
}

func (c *Client) send(b [][]any) {
	s := stream{
		Labels: c.Labels,
		Values: b,
	}
	body := postLoki{
		Streams: []stream{s},
	}
	data, err := json.Marshal(body)
	if err != nil {
		c.err = err
		return
	}
	res, err := c.c.Post(c.URL, "application/json", bytes.NewReader(data))
	if err != nil {
		c.err = err
	} else if res.StatusCode != http.StatusOK {
		c.err = errors.New("loki_client: fail sending log to loki with status: " + res.Status) // todo: maybe create error, so used decide what to do with error
	}
}

func (c *Client) Err() error {
	return c.err
}

// Stop stops sender goroutine from sending logs to [loki]
func (c *Client) Stop() {
	c.stop()
}

type stream struct {
	Labels map[string]string `json:"stream"`
	Values [][]any           `json:"values"`
}

type value struct {
	Ts   time.Time      `json:"ts"`
	Line string         `json:"line"`
	Meta map[string]any `json:"meta"`
}

func (v *value) List() []any {
	var a []any
	a = append(a, v.nano(), v.Line)
	return a
}

func (v *value) nano() string {
	return strconv.FormatInt(v.Ts.UnixNano(), 10)
}

type postLoki struct {
	Streams []stream `json:"streams"`
}

type Config struct {
	URL          string
	BatchMaxWait time.Duration
	BatchMaxSize int
	Labels       map[string]string
}

// test data for loki ingest
var testData = `
{"streams": [{ "stream": { "label": "bar2" }, "values": [ [ "1742124537511023448", "fizzbuzz info" ] ] }]}
`

// loki push api json format
// endpoint http://localhost:3100/loki/api/v1/push
// method POST
const format = `
{
  "streams": [
    {
      "stream": {
        "label": "value"
      },
      "values": [
          [ "<unix epoch in nanoseconds>", "<log line>" ],
          [ "<unix epoch in nanoseconds>", "<log line>" ]
      ]
    }
  ]
}`
