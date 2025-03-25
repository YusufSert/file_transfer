package main

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
	"path"
	"runtime"
	"strconv"
	"time"
)

func main() {
	// Fast version (changes order)
	a := []string{"A", "B", "C", "D", "E"}
	i := 2
	noew := time.Now()
	// Remove the element at index i from a.
	a[i] = a[len(a)-1] // Copy last element to index i
	a[len(a)-1] = ""   // Erase last element (write zero value)
	a = a[:len(a)-1]   // Truncate slice.
	fmt.Println(time.Since(noew))
	fmt.Println(a)

	// Slow version
	b := []string{"A", "B", "C", "D", "E"}

	noew = time.Now()
	// Remove the element at index i from b
	copy(b[i:], b[i+1:]) // Shift b[i+1:] left one index.
	b[len(b)-1] = ""     // Erase last element (write zero value).
	b = b[:len(b)-1]     // Truncate slice

	/*
		err := os.Rename("./log3.txt", "./test/log2.txt")

		if err != nil {
			var eN syscall.Errno
			if errors.As(err, &eN) {
				fmt.Println(uint(eN))
			}
			fmt.Println(eN)
		}
	*/
	/*
	   	   fileW, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	   	   if err != nil {
	   	       log.Fatal(err.Error())
	   	   }
	   	   defer fileW.Close()
	   	   l := slog.New(slog.NewJSONHandler(fileW, nil)).WithGroup("data")
	   	   for i := 0; i < 10; i++ {
	   	       l.Error("error log", "err", errors.New("test-error"))
	   	       l.Info("info log", "id", 1000+i)
	   	   }
	       	file, err := os.Open(filePath)
	       	if err != nil {
	       		log.Fatal(err)
	       	}

	       	reader := bufio.NewReader(file)
	       	var pos uint64
	       	for {
	       		record, err := reader.ReadBytes('\n')
	       		if err != nil {
	       			if err == io.EOF {
	       				time.Sleep(1 * time.Second)
	       				fmt.Println(err)
	       			}
	       		}
	       		pos += uint64(len(record))
	       		fmt.Println(pos)

	       		if err == nil {
	       			var r Record
	       			err = json.Unmarshal(record, &r)
	       			if err != nil {
	       				log.Fatal(err)
	       			}
	       			fmt.Printf("%+v\n", r)
	       		}
	       	}
	*/

	fmt.Println(time.Now().Format("2006"))
}

var base string = "./SANTRAL/ORTAK/SCS/OFIS_DESTEK/POLIS/PGM"

type bok struct {
	year int
}

// todo: maybe implement function that returns dir.
func (b *bok) test(dir string) (string, error) {
	currYear := time.Now().Year()
	var fullPath string
	if b.year == currYear {
		fullPath = path.Join(base, strconv.Itoa(b.year), dir)
		return fullPath, nil
	}

	b.year = currYear

	fullPath = path.Join(base, strconv.Itoa(b.year), dir)
	err := os.MkdirAll(fullPath, 0750)
	if err == nil || errors.Is(err, os.ErrExist) {
		return fullPath, nil
	}
	return "", err
}

type LogAgent struct {
	db     *sql.DB
	Logger *slog.Logger
	Level  *slog.LevelVar
	r      *bufio.Reader

	position uint64
	fileStat os.FileInfo
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
		Logger: logger,
		Level:  level,
		r:      reader,
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
	a.Logger.Info("log-agent: agent starting to read log file", "file_stat", a.fileStat)
	backoff, backOffMax := time.Second*3, time.Second*60
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
			}

			err = a.writeToDB(r) // if it fails forget it not that big deal
			if err != nil {
				a.Logger.Error("log-agent: error writing record to db", "error", err)
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
	w.Write([]byte("new log level" + a.Level.String()))
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

func stack() string {
	var buf [2 << 10]byte
	return string(buf[:runtime.Stack(buf[:], false)])
}
