package services

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"github.com/kr/fs"
	"io"
	"log/slog"
	"os"
	"path"
	"pgm/filetransfer"
	"pgm/tools"
	"strings"
	"sync"
	"syscall"
	"time"
)

// todo: check osPipe, ioTeeReader

type PGMService struct {
	ftp      *filetransfer.FTP
	r        *sql.DB
	l        *slog.Logger
	renameFn func(name string) string
	cfg      PGMConfig

	mu                sync.Mutex //protects the following fields
	maxTimeoutStopped int64      // Total number of workers stopped due to timout.
	ErrStopped        int64      // Total number of workers stopped due to err.
}

func NewPGMService(cfg PGMConfig, l *slog.Logger) (*PGMService, error) {
	f, err := filetransfer.Open(cfg.Addr, cfg.User, cfg.Password)
	if err != nil {
		return nil, err
	}

	return &PGMService{
		ftp: f,
		cfg: cfg,
		l:   l,
	}, nil
}

func (s *PGMService) Run(ctx context.Context) error {
	errLocal := s.monitor(ctx, s.syncLocal, "syncLocal")
	errServer := s.monitor(ctx, s.syncServer, "syncServer")

	// use monitor for restarting, alerting the user and send the error to Run() if error not retryable.
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

// todo: alert user that new file syncing
// syncLocal syncs local files by pooling the ftp-server with s.cfg.PoolInterval
func (s *PGMService) syncLocal(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	logger := s.l.With("worker", "syncLocal")
	logger.Info("pgm: syncing local files", "src", path.Join(s.cfg.Addr, s.cfg.FTPReadPath), "dst", s.cfg.NetworkIncomingPath)

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)

	go func() {
		defer close(errCh)
		defer close(heartbeatCh)
		localP := s.cfg.NetworkIncomingPath

		poolTimer := time.NewTimer(s.cfg.PoolInterval)
		pulse := time.NewTicker(d)
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
				logger.Debug("pgm: pooling", "src", path.Join(s.cfg.Addr, s.cfg.FTPReadPath), "dst", s.cfg.NetworkIncomingPath)
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}

			// sync-list
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
				logger.Info("pgm: trying to sync file", "ftp_file_name", i.Name())

				name := path.Join(localP, i.Name())
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
				f, err := createFile(name) // if windows err-code 53 then we should rtry
				if err != nil {
					errCh <- err
					return
				}

				_, err = s.ftp.Copy(f, path.Join(s.cfg.FTPReadPath, i.Name()))
				if err != nil {
					errCh <- &ServiceError{Msg: "pgm: couldn't copy " + i.Name() + " from ftp server", Op: "s.ftp.Copy", Trace: tools.Stack(), Retry: true, Err: err}
					return
				}

				nN, err := newName(f)
				if err != nil {
					errCh <- &ServiceError{Msg: "pgm: couldn't create newName for file: " + path.Base(f.Name()), Op: "newName", Trace: tools.Stack(), Retry: false, Err: err}
					return
				}

				nP := path.Join(localP, nN)
				err = os.Rename(f.Name(), nP)
				if err != nil {
					errCh <- &ServiceError{Msg: fmt.Sprintf("pgm: couldn't rename oldpath: %s, newPath: %s", f.Name(), nP), Op: "os.Rename", Trace: tools.Stack(), Retry: false, Err: err} // not retryable error
					return
				}
				f.Close()

				if !debugSync {
					//todo: ftp.Delete can return not found error 550 code
					err = s.ftp.Delete(path.Join(s.cfg.FTPReadPath, i.Name()))
					if err != nil {
						errCh <- &ServiceError{Msg: "pgm: couldn't delete " + i.Name() + " from the server", Op: "s.ftp.Delete", Trace: tools.Stack(), Retry: true, Err: err}
						return
					}
				}

				logger.Info("pgm: file synced", "ftp_file_name", i.Name(), "local_file_name", f.Name())
			}
			if !poolTimer.Stop() {
				select {
				case <-poolTimer.C:
				default:
				}
			}
			poolTimer.Reset(s.cfg.PoolInterval)
		}
	}()

	return heartbeatCh, errCh
}

// syncServer reads NetworkToUploadPath and writes files to FTPWritePath and moves them to NetworkOutgoingPath
func (s *PGMService) syncServer(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	logger := s.l.With("worker", "syncServer")
	logger.Info("pgm: syncing server files", "src", s.cfg.NetworkToUploadPath, "dst", path.Join(s.cfg.Addr, s.cfg.FTPWritePath))

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)

	go func() {
		defer close(errCh)
		defer close(heartbeatCh)

		localP := s.cfg.NetworkToUploadPath

		poolTimer := time.NewTimer(s.cfg.PoolInterval)
		pulse := time.NewTicker(d)
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
				logger.Debug("pgm: pooling", "src", s.cfg.NetworkToUploadPath, "dst", path.Join(s.cfg.Addr, s.cfg.FTPWritePath))
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}

			// sync-list
			infos, err := s.listFiles(localP)
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
				logger.Info("pgm: trying to sync file", "local_file_name", i.Name())

				//todo: try all io operations with vpn open and close and see the erros, bc windows uses networkDirs
				f, err := openFile(path.Join(localP, i.Name()))
				if err != nil {
					errCh <- err
					return
				}

				name := path.Join(s.cfg.FTPWritePath, i.Name())
				err = s.ftp.Store(name, f)
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't store %s to server %w", i.Name(), err) // todo: change this to ServiceError{}
					return
				}

				err = move(f.Name(), path.Join(s.cfg.NetworkOutgoingPath, i.Name()))
				if err != nil {
					errCh <- err
					f.Close()
					return
				}
				f.Close()

				//remove yapamıyor cunku move() zaten file ı kaldırdı bunu test error ları için bırak kalsın burda error istediğinde bunu uncommnet yap
				/*
					err = remove(path.Join(localP, i.Name()))
					if err != nil {
						errCh <- err
						return
					}
				*/
				logger.Info("pgm: file synced", "local_file_name", f.Name(), "ftp_file_name", i.Name())
			}
			if !poolTimer.Stop() {
				select {
				case <-poolTimer.C:
				default:
				}
			}
			poolTimer.Reset(s.cfg.PoolInterval)
		}
	}()

	return heartbeatCh, errCh
}

type worker func(context.Context, time.Duration) (<-chan struct{}, <-chan error)

// monitor, monitors the worker and restart the worker if need it
func (s *PGMService) monitor(ctx context.Context, fn worker, wName string) <-chan error {
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
			logger.Debug("pgm: staring worker: " + wName)
			workerHeartbeat, workerErrCh = fn(dctx, s.cfg.HeartBeatInterval)
		}
		startWorker()

		timeout := time.NewTimer(5 * time.Second)
		for {
			select {
			case <-workerHeartbeat:
				logger.Debug("pgm: receiving heartbeat from worker:" + wName)
			case <-timeout.C:
				logger.Warn("pgm: heartbeat timeout, unhealthy goroutine; restarting: " + wName)

				s.mu.Lock()
				s.maxTimeoutStopped++
				s.mu.Unlock()
				// todo: alert user
				cancel()
				startWorker()
			case err := <-workerErrCh: // if not retryable error stop monitoring.
				// todo: alert user
				// dont send the error directly check if retryable error.
				//errCh <- err
				logger.Error("pgm: "+wName+" worker failure, cancelling the worker", "err", err)
				s.mu.Lock()
				s.ErrStopped++
				s.mu.Unlock()

				cancel()
				startWorker()
				logger.Info("pgm: worker restarted")
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

func (s *PGMService) moveTo(r io.Reader, name string) error {
	f, err := os.Create(name)
	if err != nil {
		return fmt.Errorf("pgm: couldn't move the file %w", err)
	}
	defer f.Close()

	_, err = f.ReadFrom(r)
	if err != nil {
		return fmt.Errorf("pgm: couldn't move the file %w", err)
	}
	return nil
}

func (s *PGMService) listFiles(root string) ([]os.FileInfo, error) {
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

//
//All I/O operations bad network failure error detail abstracted away from caller by wrapping them by another function
//

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
func newName(f *os.File) (string, error) {
	f.Seek(0, 0)
	hash, err := getHash(f)
	if err != nil {
		return "", err
	}

	clean, _ := strings.CutSuffix(path.Base(f.Name()), path.Ext(f.Name()))
	clean = strings.ReplaceAll(clean, " ", "_")
	ext := ".QRP"
	return fmt.Sprintf("%s_x%s%s", clean, hash, ext), nil
}

func getHash(w io.WriterTo) (string, error) {
	h := sha1.New()
	_, err := w.WriteTo(h)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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

func (s *PGMService) alertIncomingFile() {
	panic("not implemented!")
}

type PGMConfig struct {
	User, Password       string
	Addr                 string
	NetworkToUploadPath  string
	NetworkOutgoingPath  string
	NetworkIncomingPath  string
	NetworkDuplicatePath string
	FTPWritePath         string
	FTPReadPath          string
	PoolInterval         time.Duration
	HeartBeatInterval    time.Duration
}
