package main

import (
	"encoding/json"
	"errors"
	"github.com/jlaffaye/ftp"
	"github.com/spf13/viper"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"
)

func main() {
	v, err := readConfig() // no need to learn editor no need to know c#, yaml, json, remote
	if err != nil {
		log.Fatal(err)
	}
	user := v.GetString("user")
	pass := v.GetString("password")
	host := v.GetString("host")
	FTPReadPath := v.GetString("FTPReadPath")

	service, err := newFTP(user, pass, host, FTPReadPath)
	if err != nil {
		log.Fatal(err)
	}
	go service.runLog() // runs concurrently

	http.HandleFunc("GET /entries", service.getFiles)
	http.HandleFunc("GET /changeLevel", service.changeLevel)
	http.ListenAndServe("localhost:8080", nil)
}

func newFTP(user, pass, host, FTPReadPath string) (*TestFTP, error) {
	c, err := ftp.Dial(host + ":21")
	if err != nil {
		return nil, err
	}

	err = c.Login(user, pass)
	if err != nil {
		return nil, err
	}

	s := &TestFTP{}
	s.level = &slog.LevelVar{}
	opts := slog.HandlerOptions{Level: s.level}
	s.l = slog.New(slog.NewJSONHandler(os.Stdout, &opts))
	s.conn = c
	return s, nil
}

func (s *TestFTP) getFiles(w http.ResponseWriter, r *http.Request) {
	entries, err := s.readFTP(s.FTPReadPath)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (s *TestFTP) readFTP(rootPath string) ([]*ftp.Entry, error) {
	entries := make([]*ftp.Entry, 0)
	w := s.conn.Walk(rootPath)
	for w.Next() {
		if err := w.Err(); err != nil {
			return nil, err
		}

		entry := w.Stat()
		if entry.Type == ftp.EntryTypeFolder {
			w.SkipDir()
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (s *TestFTP) runLog() {
	for {
		s.l.Error("error log", "err", errors.New("error"), "stack", stack())
		s.l.Debug("debug log", "op", "starting local sync")
		time.Sleep(5 * time.Second)
	}
}
func (s *TestFTP) changeLevel(w http.ResponseWriter, r *http.Request) {
	if s.level.Level() == slog.LevelDebug {
		s.level.Set(slog.LevelError)
		w.Write([]byte("LevelError"))
		return
	}
	s.level.Set(slog.LevelDebug)
	w.Write([]byte("LevelDebug"))
}

type TestFTP struct {
	conn        *ftp.ServerConn
	l           *slog.Logger
	level       *slog.LevelVar
	FTPReadPath string
}

func readConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("/etc/pgm")     // linux config dir.
	v.AddConfigPath("./demo_sunum") // optionally, look for config in the working dir.
	err := v.ReadInConfig()         // find and read the config file
	if err != nil {
		return nil, err
	}
	return v, nil
}
func stack() string {
	var buf [1 << 10]byte
	return string(buf[:runtime.Stack(buf[:], false)])
}
