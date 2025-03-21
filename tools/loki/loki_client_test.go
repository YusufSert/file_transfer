package loki

import (
	"testing"
	"time"
)

func TestLokiClient(t *testing.T) {
	client, err := NewClient(Config{
		URL:          "http://localhost:3100/loki/api/v1/push",
		BatchMaxSize: 100,
		BatchMaxWait: 1 * time.Second,
		Labels:       map[string]string{"test": "test", "service_name": "test_loki_client"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logs []string = []string{"fizz buzz debug", "foo, bar warn", "kudimmmm error"}
	for _, l := range logs {
		client.Send(l) // sends logs to sender goroutine.
	}
	time.Sleep(2 * time.Second) // wait BachMaxWait + 1sec to make sure batch send to loki.
	if err = client.Err(); err != nil {
		t.Fatal(err)
	}
}
