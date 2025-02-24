package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/kr/fs"
	"io"
	"log/slog"
	"os"
	"path"
	"pgm/filetransfer"
	"sync"
	"time"
)

// todo check osPipe, ioTeeReader

type PGMService struct {
	ftp    *filetransfer.FTP
	r      *sql.DB
	l      *slog.Logger
	rename func(name string) string
	cfg    PGMConfig
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

func NewPGMService(cfg PGMConfig) (*PGMService, error) {
	f, err := filetransfer.Open(cfg.Addr, cfg.User, cfg.Password)
	if err != nil {
		return nil, err
	}
	// todo fake logger lib deki default config func burda kullan
	return &PGMService{
		ftp: f,
		cfg: cfg,
		l:   slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}, nil
}

func (s *PGMService) Run(ctx context.Context) error {
	errLocal, pulseLocal := s.syncLocal(ctx, time.Second*3)
	errServer, pulseServer := s.syncServer(ctx, time.Second*3)

	var err error
	for err == nil {
		select {
		case err = <-errLocal:
			// restart localSync
		case err = <-errServer:
			// restart serverSync
		default:
			select {
			case <-pulseLocal:
				fmt.Println("local sync alive")
			case <-time.After(time.Second * 5):
				// todo: restart both sync
			}
			select {
			case <-pulseServer:
				fmt.Println("server sync alive")
			case <-time.After(time.Second * 5):
				// todo: restart sync
			}
		}
	}
	return err
}

func (s *PGMService) syncLocal(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	s.l.Debug("pgm: syncing local files", "src", path.Join(s.cfg.Addr, s.cfg.FTPReadPath), "dst", s.cfg.NetworkIncomingPath)

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		defer close(heartbeatCh)

		t := time.NewTimer(d)
		localP := s.cfg.NetworkIncomingPath
		for {
			select {
			case <-t.C:
			case <-time.After(time.Second * 1):
				select {
				default:
				case heartbeatCh <- struct{}{}:
				}
				continue
			}

			// sync-list
			infos, err := s.ftp.ListFilesContext(ctx, s.cfg.FTPReadPath)
			if err != nil {
				errCh <- fmt.Errorf("pgm: couldn't fetch file infos from server %w", err)
				return
			}

			for _, i := range infos {
				select {
				default:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
				// send pulse to say I am alive.

				name := path.Join(localP, i.Name())
				// Don't check if file exists on local, server file will be deleted and of this process.
				// Next pool it will be not on the sync-list.
				/*
					_, err = os.Stat(name)
					// if file not exists on local dir sync from server, otherwise log the error and continue.
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						slog.Error("pgm: couldn't check if file exists on local dir", "file_path", name, "err", err)
						continue
					}
				*/
				f, err := os.Create(s.rename(name))
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't create %s on local machine %w", name, err)
					return
				}

				_, err = s.ftp.Copy(f, i.Name())
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't copy %s from ftp server %w", i.Name(), err)
					return
				}
				f.Close()

				err = s.ftp.Delete(i.Name())
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't delete %s from the server %w", i.Name(), err)
					return
				}
			}
			t.Reset(d)
		}
	}()

	return heartbeatCh, errCh
}

// syncServer reads NetworkToUploadPath and writes files to FTPWritePath and moves them to NetworkOutgoingPath
func (s *PGMService) syncServer(ctx context.Context, d time.Duration) (<-chan struct{}, <-chan error) {
	s.l.Debug("pgm: syncing server files", "src", s.cfg.NetworkToUploadPath, "dst", path.Join(s.cfg.Addr, s.cfg.FTPWritePath))

	heartbeatCh := make(chan struct{}, 1)
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		defer close(heartbeatCh)

		t := time.NewTimer(d)
		localP := s.cfg.NetworkToUploadPath
		for {
			select {
			case <-t.C:
			}

			// sync-list
			infos, err := s.listFiles(localP)
			if err != nil {
				errCh <- fmt.Errorf("pgm: couldn't list file infos from local machine %w", err)
				return
			}

			for _, i := range infos {
				select {
				default:
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
				// send pulse to say I am alive.
				select {
				case heartbeatCh <- struct{}{}:
				default:
				}

				f, err := os.Open(path.Join(localP, i.Name()))
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't open %s on local machine %w", i.Name(), err)
					return
				}

				name := path.Join(s.cfg.FTPWritePath, i.Name())
				err = s.ftp.Store(name, f)
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't store %s to server %w", i.Name(), err)
					return
				}

				err = s.moveTo(f, path.Join(s.cfg.NetworkOutgoingPath, i.Name()))
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't move %s to %s %w", i.Name(), s.cfg.NetworkOutgoingPath, err)
					f.Close()
					return
				}
				f.Close()

				err = os.Remove(path.Join(localP, i.Name()))
				if err != nil {
					errCh <- fmt.Errorf("pgm: couldn't remove %s from %s %w", i.Name(), s.cfg.NetworkToUploadPath, err)
					return
				}
			}
			t.Reset(d)
		}
	}()

	return heartbeatCh, errCh
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
	for w.Step() {
		if err := w.Err(); err != nil {
			return nil, err
		}

		info := w.Stat()
		if info.IsDir() {
			continue
		}
		fileInfos = append(fileInfos, info)
	}

	return fileInfos, nil
}

type PGMError struct {
	Op  string
	err error
}

type Circuit func(ctx context.Context) error

func Breaker(circuit Circuit, failureThreshold uint) Circuit {
	consecutiveFailures := 0
	lastAttempt := time.Now()
	var m sync.RWMutex

	return func(ctx context.Context) error {
		m.RLock()

		d := consecutiveFailures - int(failureThreshold)

		if d >= 0 {
			shouldRetryAt := lastAttempt.Add(time.Second * 2 << d)
			if !time.Now().After(shouldRetryAt) {
				m.RUnlock()
				return errors.New("service unreachable")
			}
		}

		m.RUnlock() // Release read lock

		err := circuit(ctx)

		m.Lock() // Lock around shared resources
		defer m.Unlock()

		lastAttempt = time.Now() // Record tme of attempt

		if err != nil { // Circuit returned an error, so we count the failure and return
			consecutiveFailures++
			return err
		}

		consecutiveFailures = 0 // Reset failures counter

		return nil
	}
}
