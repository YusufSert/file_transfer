package app

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"github.com/kr/fs"
	"github.com/spf13/viper"
	"io"
	"log/slog"
	"os"
	"path"
	"pgm/filetransfer"
	"pgm/repo"
	"pgm/tools"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// todo: check osPipe, ioTeeReader

type PGM struct {
	ftp      *filetransfer.FTP
	r        *repo.PGMRepo
	l        *slog.Logger
	renameFn func(name string) string
	cfg      *PGMConfig

	mu                sync.Mutex //protects the following fields
	maxTimeoutStopped int64      // Total number of workers stopped due to timout.
	ErrStopped        int64      // Total number of workers stopped due to err.
	year              int        // current file year for pgm files.
}

func NewPGMService(cfg *PGMConfig, r *repo.PGMRepo, l *slog.Logger) (*PGM, error) {
	f, err := filetransfer.Open(cfg.Addr, cfg.User, cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("pgm: error opening ftp conn %w", err)
	}

	return &PGM{
		ftp: f,
		cfg: cfg,
		l:   l,
		r:   r,
	}, nil
}

func (s *PGM) Run(ctx context.Context) error {
	errLocal := s.monitor(ctx, s.syncLocal, "syncLocal")
	errServer := s.monitor(ctx, s.syncServer, "syncServer")

	var err error
	for {
		select {
		case err = <-errLocal:
			return err
		case err = <-errServer:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var debugSync bool = true

// syncLocal syncs local files by pooling the ftp-server with s.cfg.PoolInterval
func (s *PGM) syncLocal(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	logger := s.l.With("worker", "syncLocal")

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)

	go func() {
		defer close(errCh)
		defer close(heartbeatCh)

		poolTimer := time.NewTimer(d)
		pulse := time.NewTicker(s.cfg.HeartBeatInterval)
		defer poolTimer.Stop()
		defer pulse.Stop()

		for {
			select {
			case <-pulse.C:
				select {
				default:
				case heartbeatCh <- struct{}{}:
				}
				continue
			case <-poolTimer.C:
				logger.Debug("pgm: pooling", "src", path.Join(s.cfg.Addr, s.cfg.FTPReadPath), "dst", path.Join(s.cfg.NetworkBasePath, time.Now().Format("2006"), s.cfg.NetworkIncomingPath))
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}

			infos, err := s.ftp.ListFilesContext(ctx, s.cfg.FTPReadPath)
			if err != nil {
				errCh <- &ServiceError{Msg: "pgm: couldn't fetch file infos from server", Op: "s.ftp.ListFilesContext", Trace: tools.Stack(), Retry: true, Err: err}
				return
			}

			for _, i := range infos {
				select {
				case <-pulse.C:
					select {
					case heartbeatCh <- struct{}{}:
					default:
					}
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				default:
				}
				logger.Debug("pgm: trying to sync file", "ftp_file_name", i.Name())
				// Don't check if file exists on local, server file will be deleted and of this process.
				// Next sync it will be not on the sync-list.
				/*
					_, err = os.Stat(name)
					// if file not exists on local dir sync from server, otherwise log the error and continue.
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						slog.Error("pgm: couldn't check if file exists on local dir", "file_path", name, "err", err)
						continue
					}
				*/
				buf := &bytes.Buffer{}
				_, err = s.ftp.Copy(buf, path.Join(s.cfg.FTPReadPath, i.Name()))
				if err != nil {
					errCh <- &ServiceError{Msg: "pgm: couldn't copy " + i.Name() + " from ftp server", Op: "s.ftp.Copy", Trace: tools.Stack(), Retry: true, Err: err}
					return
				}

				newFileName, err := newName(buf.Bytes(), i.Name())
				if err != nil {
					errCh <- err
					return
				}

				s.mu.Lock()
				dirPath, err := s.getPathLocked(s.cfg.NetworkIncomingPath)
				s.mu.Unlock()
				if err != nil {
					errCh <- err
					return
				}

				f, err := createFile(path.Join(dirPath, newFileName)) // if windows err-code 53 then we should rtry
				if err != nil {
					errCh <- err
					return
				}

				_, err = io.Copy(f, buf)
				if err != nil {
					errCh <- &ServiceError{Msg: fmt.Sprintf("pgm: couldn't write to file %s", newFileName), Op: "io.Copy", Trace: tools.Stack(), Retry: true, Err: err}
					f.Close()
					return
				}
				f.Close()

                err = s.r.WriteDB(ctx, newFileName, dirPath)
                if err != nil {
                    errCh <- &ServiceError{Msg: fmt.Sprintf("pgm: couldn't save file entry to to db %s", newFileName), Op: "s.r.WriteDB", Trace: tools.Stack(), Retry: true, Err: err}
                    return
                }

				if !debugSync {
					//todo: ftp.Delete can return not found error 550 code
					err = s.ftp.Delete(path.Join(s.cfg.FTPReadPath, i.Name()))
					if err != nil {
						errCh <- &ServiceError{Msg: "pgm: couldn't delete " + i.Name() + " from the server", Op: "s.ftp.Delete", Trace: tools.Stack(), Retry: true, Err: err}
						return
					}
				}

				logger.Info("pgm: file synced", "ftp_file_name", i.Name(), "network_name", f.Name())
			}
			if !poolTimer.Stop() {
				select {
				case <-poolTimer.C:
				default:
				}
			}
			poolTimer.Reset(d)
		}
	}()

	return heartbeatCh, errCh
}

// syncServer reads NetworkToUploadPath and writes files to FTPWritePath and moves them to NetworkOutgoingPath
func (s *PGM) syncServer(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	logger := s.l.With("worker", "syncServer")

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)

	go func() {
		defer close(errCh)
		defer close(heartbeatCh)

		dirPath := path.Join(s.cfg.NetworkBasePath, time.Now().Format("2006"), s.cfg.NetworkToUploadPath)

		poolTimer := time.NewTimer(d)
		pulse := time.NewTicker(s.cfg.HeartBeatInterval)
		defer poolTimer.Stop()
		defer pulse.Stop()

		for {
			select {
			case <-pulse.C:
				select {
				case heartbeatCh <- struct{}{}:
				default:
				}
				continue
			case <-poolTimer.C:
				logger.Debug("pgm: pooling", "src", dirPath, "dst", path.Join(s.cfg.Addr, s.cfg.FTPWritePath))
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}

			// sync-list
			infos, err := s.listFiles(dirPath)
			if err != nil {
				errCh <- err
				return
			}

			for _, i := range infos {
				select {
				case <-pulse.C:
					select {
					case heartbeatCh <- struct{}{}:
					default:
					}
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				default:
				}
				logger.Debug("pgm: trying to sync file", "network_name", i.Name())

				//todo: try all io operations with vpn open and close and see the erros, bc windows uses networkDirs
				f, err := openFile(path.Join(dirPath, i.Name()))
				if err != nil {
					errCh <- err
					return
				}

				filePath := path.Join(s.cfg.FTPWritePath, i.Name())
				err = s.ftp.Store(filePath, f)
				if err != nil {
					errCh <- &ServiceError{Msg: "pgm: couldn't store" + i.Name() + "to server", Op: "s.ftp.Store", Trace: tools.Stack(), Retry: true, Err: err}
					return
				}

				s.mu.Lock()
				outDir, err := s.getPathLocked(s.cfg.NetworkOutgoingPath)
				s.mu.Unlock()

				err = move(f.Name(), path.Join(outDir, i.Name()))
				if err != nil {
					errCh <- err
					f.Close()
					return
				}
				f.Close()

				logger.Info("pgm: file synced", "network_name", f.Name(), "ftp_name", i.Name())
			}
			if !poolTimer.Stop() {
				select {
				case <-poolTimer.C:
				default:
				}
			}
			poolTimer.Reset(d)
		}
	}()

	return heartbeatCh, errCh
}

type worker func(context.Context, time.Duration) (<-chan struct{}, <-chan error)

// monitor, monitors the worker and restart the worker if need it
func (s *PGM) monitor(ctx context.Context, fn worker, wName string) <-chan error {
	logger := s.l.With("monitor", wName)
	errCh := make(chan error)

	go func() {
		defer close(errCh)

		var workerHeartbeat <-chan struct{}
		var workerErrCh <-chan error
		var cancel context.CancelFunc
		var dctx context.Context // derived context

		startWorker := func() {
			dctx, cancel = context.WithCancel(ctx)
			logger.Debug("pgm: staring worker")
			workerHeartbeat, workerErrCh = fn(dctx, s.cfg.PoolInterval)
		}
		startWorker()

		timeout := time.NewTimer(5 * time.Second)
		for {
			select {
			case <-workerHeartbeat:
				logger.Debug("pgm: receiving heartbeat from worker")
			case <-timeout.C:
				logger.Warn("pgm: heartbeat timeout, unhealthy goroutine; restarting worker")

				s.mu.Lock()
				s.maxTimeoutStopped++
				s.mu.Unlock()

				cancel()
				startWorker()
			case err := <-workerErrCh:
				// dont send the error directly check if retryable error.
				// if not retryable error stops monitoring.
				logger.Error("pgm: "+wName+" worker failure, cancelling the worker", "err", err)
				cancel()

				s.mu.Lock()
				s.ErrStopped++
				s.mu.Unlock()

				var serviceErr *ServiceError
				if !errors.As(err, &serviceErr) {
					errCh <- err
					return
				}

				if !serviceErr.Retry {
					errCh <- err
					return
				}

				logger.Info("pgm: restarting worker")
				startWorker()

			case <-ctx.Done(): // parent context will cancel the child ctx, no deed to explicitly call cancel() on the child ctx
				errCh <- ctx.Err()
				return
			}
			if !timeout.Stop() {
				select {
				case <-timeout.C:
				default:
				}
			}
			timeout.Reset(time.Second * 5)
		}
	}()
	return errCh
}

func (s *PGM) listFiles(root string) ([]os.FileInfo, error) {
	var fileInfos []os.FileInfo
	w := fs.Walk(root)
	r := false
	for w.Step() {
		if err := w.Err(); err != nil {
			if isBadNetPath(err) {
				r = true
			}
			return nil, &ServiceError{Msg: "pgm: couldn't list file infos from local machine", Op: "listFiles", Trace: tools.Stack(), Retry: r, Err: err}
		}

		info := w.Stat()
		if info.IsDir() {
			continue
		}
		fileInfos = append(fileInfos, info)
	}
	return fileInfos, nil
}

func (s *PGM) getPathLocked(dir string) (string, error) {
	currYear := time.Now().Year()
	var fullPath string
	if s.year == currYear {
		fullPath = path.Join(s.cfg.NetworkBasePath, strconv.Itoa(s.year), dir)
		return fullPath, nil
	}

	s.year = currYear

	fullPath = path.Join(s.cfg.NetworkBasePath, strconv.Itoa(s.year), s.cfg.NetworkIncomingPath)
	err := mkdir(fullPath)
	if err != nil {
		return "", err
	}
	fullPath = path.Join(s.cfg.NetworkBasePath, strconv.Itoa(s.year), s.cfg.NetworkOutgoingPath)
	err = mkdir(fullPath)
	if err != nil {
		return "", err
	}

	return path.Join(s.cfg.NetworkBasePath, strconv.Itoa(s.year), dir), nil
}

// All I/O operations bad network failure error detail abstracted away from caller by wrapping them by another function
//
// mkdir creates dir along with any necessary parents.
func mkdir(name string) error {
	r := false
	err := os.MkdirAll(name, 0750)
	if err == nil || errors.Is(err, os.ErrExist) {
		return nil
	}
	if isBadNetPath(err) {
		r = true
	}
	return &ServiceError{Msg: fmt.Sprintf("pgm: couldn't create dir %s", name), Op: "mkdir", Trace: tools.Stack(), Retry: r, Err: err}
}

// remove removes file from a given path.
func remove(name string) error {
	r := false
	err := os.Remove(name)
	if err != nil {
		if isBadNetPath(err) {
			r = true
		}
		return &ServiceError{Msg: fmt.Sprintf("pgm: couldn't remove %s", name), Op: "remove", Trace: tools.Stack(), Retry: r, Err: err}
	}
	return nil
}

func move(oldPath, newPath string) error {
	r := false
	err := os.Rename(oldPath, newPath)
	if err != nil {
		if isBadNetPath(err) {
			r = true
		}
		return &ServiceError{Msg: fmt.Sprintf("pgm: couldn't move %s to %s", oldPath, newPath), Op: "move", Trace: tools.Stack(), Retry: r, Err: err}
	}

	return nil
}

func createFile(name string) (*os.File, error) {
	f, err := os.Create(name)
	r := false
	if err != nil {
		if isBadNetPath(err) {
			r = true
		}
		return nil, &ServiceError{Msg: "pgm: couldn't create " + name + " local machine", Op: "createFile", Trace: tools.Stack(), Retry: r, Err: err}
	}

	return f, nil
}

func openFile(name string) (*os.File, error) {
	f, err := os.Open(name)
	r := false
	if err != nil {
		if isBadNetPath(err) { // check if its network-error
			r = true
		}
		return nil, &ServiceError{Msg: "pgm: couldn't open file" + name, Op: "openFile", Trace: tools.Stack(), Retry: r, Err: err}
	}

	return f, nil
}

func isBadNetPath(err error) bool {
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) && (uint(sysErr) == 53 || uint(sysErr) == 51) {
		// 53 The network path was not found.
		// 51 The remote computer is not available.
		return true
	}
	return false
}

// newName returns new name as (file-name + file-hash + .QRP)
func newName(b []byte, name string) (string, error) {
	hash, err := getHash(b)
	if err != nil {
		return "", &ServiceError{Msg: "pgm: couldn't create newName for file: " + name, Op: "newName", Trace: tools.Stack(), Retry: false, Err: err}
	}

	clean, _ := strings.CutSuffix(name, path.Ext(name))
	clean = strings.ReplaceAll(clean, " ", "_")
	return fmt.Sprintf("%s_x%s%s", clean, hash, ".QRP"), nil
}

func getHash(b []byte) (string, error) {
	h := sha1.New()
	_, err := h.Write(b)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:4]), nil
}

type ServiceError struct {
	Msg   string
	Op    string
	Trace string
	Retry bool
	Err   error
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("%s %s %s %s", e.Op, e.Msg, e.Trace, e.Err.Error())
}

func (e *ServiceError) Unwrap() error { return e.Err }

func (s *PGM) alertIncomingFile() {
	panic("not implemented!")
}

type PGMConfig struct {
	User, Password       string
	Addr                 string
	DBConnStr            string
	NetworkToUploadPath  string
	NetworkOutgoingPath  string
	NetworkIncomingPath  string
	NetworkDuplicatePath string
	NetworkBasePath      string
	FTPWritePath         string
	FTPReadPath          string
	PoolInterval         time.Duration
	HeartBeatInterval    time.Duration
	LogFilePath          string
	LokiPushURl          string
	LogLevel             slog.Level
}

func ReadPGMConfig(path string) (*PGMConfig, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(path)
	err := v.ReadInConfig()
	if err != nil {
		return nil, err
	}

	for _, key := range v.AllKeys() {
		if !v.IsSet(key) {
			return nil, fmt.Errorf("pgm: %s not set", key)
		}
	}

	poolInterval, err := time.ParseDuration(v.GetString("poolInterval"))
	if err != nil {
		return nil, fmt.Errorf("pgm: couldn't parse poolInterval %s", v.GetString("poolInterval"))
	}
	heartbeatInterval, err := time.ParseDuration(v.GetString("heartBeatInterval"))
	if err != nil {
		return nil, fmt.Errorf("pgm: couldn't parse heartBeatInterval %s", v.GetString("heartBeatInterval"))
	}

	logLevel := v.GetInt("logLevel")

	return &PGMConfig{
		User:                 v.GetString("user"),
		Password:             v.GetString("password"),
		Addr:                 v.GetString("addr"),
		DBConnStr:            v.GetString("dbConnStr"),
		NetworkToUploadPath:  v.GetString("networkToUploadPath"),
		NetworkOutgoingPath:  v.GetString("networkOutgoingPath"),
		NetworkIncomingPath:  v.GetString("networkIncomingPath"),
		NetworkDuplicatePath: v.GetString("networkDuplicatePath"),
		NetworkBasePath:      v.GetString("networkBasePath"),
		FTPWritePath:         v.GetString("ftpWritePath"),
		FTPReadPath:          v.GetString("ftpReadPath"),
		LogFilePath:          v.GetString("logFilePath"),
		LokiPushURl:          v.GetString("lokiPushURL"),
		LogLevel:             slog.Level(logLevel),
		PoolInterval:         poolInterval,
		HeartBeatInterval:    heartbeatInterval,
	}, nil
}
