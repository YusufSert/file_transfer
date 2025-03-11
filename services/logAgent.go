package services

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type LogAgent struct {
	db     *sql.DB
	Logger *slog.Logger
	Level  *slog.LevelVar
	r      *bufio.Reader

	position uint64
	filePath string
	closed   bool
	err      error
}

func (a *LogAgent) Close() error {
	a.closed = true
	return nil
}
func (a *LogAgent) Err() error {
	return a.err
}

var filePath string = "./log2.txt"

func NewLogAgent() (*LogAgent, error) {
	fileW, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	fileR, err := os.OpenFile(filePath, os.O_RDONLY, 0644)
	if err != nil {
		return nil, err
	}

	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	opts := &slog.HandlerOptions{Level: level}
	logger := slog.New(slog.NewJSONHandler(fileW, opts)).WithGroup("data")
	reader := bufio.NewReader(fileR)

	a := &LogAgent{
		Logger:   logger,
		Level:    level,
		r:        reader,
		filePath: filePath,
	}

	return a, nil
}

func (a *LogAgent) Run(ctx context.Context) error {
	go a.runServer()
	// creates end point for changing the log level.
	// if runServer() fails a.Closed() called and a.err set the caused error.
	a.pool(ctx)
	return a.err
}

func (a *LogAgent) pool(ctx context.Context) <-chan struct{} {
	a.Logger.Info("log-agent: agent starting to read log file", "file_path", a.filePath)
	backoff, backOffMax := time.Millisecond*300, time.Second*60
	d := backoff

	heartbeat := make(chan struct{}, 1)
	go func() {
		defer close(heartbeat)
		t := time.NewTimer(d)

		for !a.closed {
			select {
			case <-time.After(1 * time.Second):
				select {
				case heartbeat <- struct{}{}:
				default:
				}
				continue
			case <-t.C:
			case <-ctx.Done():
				a.err = ctx.Err()
				return
			}

			record, err := a.r.ReadBytes('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) {
					a.Logger.Error("log-agent: couldn't read new log file", "error", err)
					a.err = err
					break
				}
				d = d << 1 //backoff, backoff_factor, max_backoff
				if d > backOffMax {
					d = backOffMax
				}
			}

			var r Record
			if err == nil {
				a.position += uint64(len(record))
				err = json.Unmarshal(record, &r) // todo: stop runner and seek to end of file maybe.
				if err != nil {
					a.Logger.Error("log-agent: error decoding record", "error", err)
					a.err = err
					break
				}

				err = a.writeToDB(r) // if it fails forget it not that big deal
				if err != nil {
					a.Logger.Error("log-agent: error writing record to db", "error", err)
				} else {
					d = backoff
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
	}()

	return heartbeat
}

func (a *LogAgent) writeToDB(r Record) error {
	fmt.Println(r)
	return nil
}

func (a *LogAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lvl := a.Level.Level() // -4, 0, 4, 8
	switch lvl {           //clean
	case slog.LevelDebug:
		lvl = slog.LevelInfo
	case slog.LevelInfo:
		lvl = slog.LevelWarn
	case slog.LevelWarn:
		lvl = slog.LevelError
	case slog.LevelError:
		lvl = slog.LevelDebug
	}
	a.Level.Set(lvl)
	stat, _ := os.Stat(a.filePath)

	info := struct {
		Level           string    `json:"log_level"`
		CurReadPosition uint64    `json:"cur_read_position"`
		FileName        string    `json:"file_name"`
		FileSize        int64     `json:"file_size"`
		FileModTime     time.Time `json:"file_mod_time"`
	}{
		Level:           a.Level.String(),
		FileName:        stat.Name(),
		FileSize:        stat.Size(),
		FileModTime:     stat.ModTime(),
		CurReadPosition: a.position,
	}

	json.NewEncoder(w).Encode(&info)
}

func (a *LogAgent) runServer() {
	err := http.ListenAndServe("localhost:8000", a)
	if err != nil {
		a.Logger.Error("http server not  running", "err", err)
		a.err = fmt.Errorf("log-agent: error creating endpoint for changing logLelel %w", err)
		a.Close()
	}
}

type Record struct {
	Time  time.Time
	Level string
	Msg   string
	Data  map[string]any
}
